package main

import (
	"fmt"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file owns the two declarations that put files, rather than environment
// variables, into a deployed pod: spec.config-mounts (a ConfigMap or Secret
// projected read-only onto a container path) and spec.service-account-tokens
// (a projected ServiceAccount token with a declared audience). Both exist
// because a workload that authenticates to something outside the cluster — a
// cell Vault reached with Kubernetes auth is the worked example in
// docs/deployment-mounts.md — needs a CA file on disk and a token minted for
// that audience, and neither can be an environment variable.
//
// Everything here resolves at Deploy time into the render types the Deployment
// template consumes. The template renders exactly what is resolved and decides
// nothing, so a value that is wrong is refused here, loudly, rather than
// surfacing as a rejected apply in a consumer's GitOps run.

// reservedVolumes are the pod volumes templates/deployment already renders,
// keyed by volume name. A declaration reusing one of these names makes the API
// server reject the pod for duplicate volume names; one reusing the path would
// shadow the scratch volume the read-only root filesystem depends on.
var reservedVolumes = map[string]string{"tmp": "/tmp"}

// dns1123Label matches a Kubernetes volume name. Unlike a ServiceAccount name
// (a DNS-1123 *subdomain*, see dns1123Subdomain in main.go) a volume name may
// not contain dots.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// nonLabelCharacters is every run of characters a DNS-1123 label may not hold.
var nonLabelCharacters = regexp.MustCompile(`[^a-z0-9]+`)

// octalFileMode matches the only spelling of a file mode this agent accepts: a
// leading 0 (or 0o) and exactly three octal digits.
//
// The leading zero is not decoration. A service spec is read into a
// map[string]any and re-marshalled before it reaches these settings
// (resources.Service.LoadSettingsFromSpec), so an UNQUOTED `mode: 0644` is
// resolved as the YAML 1.1 octal integer 420 and arrives here as the text
// "420" — which, read back as octal, is 0420: a different mode, and a valid
// one. Requiring the leading zero turns that round-trip into a refusal instead
// of a silent downgrade.
var octalFileMode = regexp.MustCompile(`^0o?[0-7]{3}$`)

// defaultFileMode is 0440: readable by the owner and the group, by nothing
// else, writable and executable by no one. See validateFileMode for why group
// read is the half that matters.
const defaultFileMode = "0440"

// Bounds on a projected ServiceAccount token's lifetime.
//
// minTokenExpirationSeconds is the API server's floor, not a policy of ours:
// corev1.ServiceAccountTokenProjection documents expirationSeconds as
// "Defaults to 1 hour and must be at least 10 minutes", and a pod asking for
// less is rejected at apply time. Refusing it here names the field at deploy
// time instead.
//
// maxTokenExpirationSeconds is this agent's ceiling. A projected token is a
// bearer credential on a pod's disk and the kubelet rotates the file at 80% of
// its lifetime, so a short lifetime costs nothing while a long one widens the
// window in which a leaked copy still authenticates.
const (
	minTokenExpirationSeconds     int64 = 600
	maxTokenExpirationSeconds     int64 = 3600
	defaultTokenExpirationSeconds int64 = 600
)

// ConfigMount projects a ConfigMap or a Secret into the deployed pod as
// read-only files under MountPath. This is the file-based configuration seam —
// a CA bundle, a trust store, a document the process reads at startup — as
// opposed to envFrom, which can only project keys as environment variables.
//
// The ConfigMap or Secret is NAMED here, never created here: it is supplied
// out-of-band per environment (a cell's workload-CA stage writes it, say), so
// the workload blocks until it exists, or starts anyway when Optional is set.
type ConfigMount struct {
	// Name is the pod volume backing this mount and must be a DNS-1123 label.
	// Left empty it is derived from the ConfigMap or Secret name.
	Name string `yaml:"name,omitempty"`

	// ConfigMap names the ConfigMap to project. Exactly one of ConfigMap or
	// Secret must be set.
	ConfigMap string `yaml:"config-map,omitempty"`

	// Secret names the Secret to project. Exactly one of ConfigMap or Secret
	// must be set.
	Secret string `yaml:"secret,omitempty"`

	// MountPath is the absolute container DIRECTORY the keys are projected
	// into: a ConfigMap key `ca.crt` mounted at `/etc/vault/tls` is the file
	// `/etc/vault/tls/ca.crt`. Whole keys only — there is no subPath, because a
	// subPath mount is never refreshed when its ConfigMap changes.
	MountPath string `yaml:"mount-path"`

	// Mode is the projected files' permissions as a quoted octal string,
	// defaulting to "0440". See validateFileMode.
	Mode string `yaml:"mode,omitempty"`

	// ReadOnly mounts the volume read-only and defaults to true. An explicit
	// false is REFUSED rather than honored: the container runs with
	// readOnlyRootFilesystem, and a writable configuration mount is a way for
	// the process to rewrite what it later re-reads. The field exists so that
	// declaring the default is allowed and declaring the opposite is answered,
	// rather than silently ignored.
	ReadOnly *bool `yaml:"read-only,omitempty"`

	// Optional lets the pod start when the ConfigMap or Secret is absent
	// instead of blocking on it, and defaults to false: a workload whose
	// configuration has not arrived should wait visibly rather than boot
	// without it.
	Optional bool `yaml:"optional,omitempty"`
}

// UnmarshalYAML decodes a config mount with unknown fields refused.
//
// A mount is the one declaration here where a dropped key changes a security
// property in silence: `host-path:` would mount the node's filesystem if
// anything honored it, a misspelt `read-only:` reads as the default, and either
// way the service's manifest claims something the pod does not do. Naming the
// unknown field is how a service author finds out, at deploy time.
func (m *ConfigMount) UnmarshalYAML(node *yaml.Node) error {
	if err := refuseUnknownFields("config-mounts", node, ConfigMount{}); err != nil {
		return fmt.Errorf("%w; a mount names a config-map or a secret, never a host path", err)
	}
	type plain ConfigMount
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*m = ConfigMount(decoded)
	return nil
}

// ServiceAccountToken projects a ServiceAccount token with a declared audience
// into the pod at Path.
//
// This is NOT the pod's API-server token: automountServiceAccountToken stays
// false, and the audience is mandatory precisely because an omitted one makes
// Kubernetes mint a token for the API server itself
// (corev1.ServiceAccountTokenProjection: "The audience defaults to the
// identifier of the apiserver"). A consumer such as Vault's Kubernetes auth
// method accepts only a token minted for its own audience, so an undeclared
// audience produces a pod that deploys cleanly and then fails every login.
type ServiceAccountToken struct {
	// Name is the pod volume backing this token and must be a DNS-1123 label.
	// Left empty it is derived from the audience.
	Name string `yaml:"name,omitempty"`

	// Audience is the intended recipient of the token, and is mandatory.
	Audience string `yaml:"audience"`

	// Path is the absolute path of the token FILE in the container — the value
	// the process is configured with (VAULT_K8S_TOKEN_PATH, say). The volume is
	// mounted at its directory and the token projected as its base name; there
	// is no subPath.
	Path string `yaml:"path"`

	// ExpirationSeconds is the requested token lifetime, defaulting to 600.
	// Kubernetes rejects anything below 600 and this agent refuses anything
	// above 3600.
	ExpirationSeconds int64 `yaml:"expiration-seconds,omitempty"`
}

// UnmarshalYAML decodes a token declaration with unknown fields refused, for
// the same reason ConfigMount does: a dropped `audience` is the difference
// between a token the consumer accepts and the pod's API-server token.
func (t *ServiceAccountToken) UnmarshalYAML(node *yaml.Node) error {
	if err := refuseUnknownFields("service-account-tokens", node, ServiceAccountToken{}); err != nil {
		return err
	}
	type plain ServiceAccountToken
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*t = ServiceAccountToken(decoded)
	return nil
}

// refuseUnknownFields reports the first key in node that spec does not declare,
// naming the fields that are accepted.
//
// yaml.Node.Decode ignores an unknown key outright and a strict decoder names
// the Go type it was decoding into, so the check happens here, where the
// message can be about the block the service author actually wrote. The
// accepted names come from the struct's own yaml tags, so they cannot drift
// from the fields.
func refuseUnknownFields(block string, node *yaml.Node, spec any) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("%s entry must be a mapping of fields", block)
	}
	fields := reflect.TypeOf(spec)
	known := make(map[string]bool, fields.NumField())
	accepted := make([]string, 0, fields.NumField())
	for i := 0; i < fields.NumField(); i++ {
		name, _, _ := strings.Cut(fields.Field(i).Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		known[name] = true
		accepted = append(accepted, name)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if key := node.Content[i].Value; !known[key] {
			return fmt.Errorf("%s entry declares unknown field %q; accepted fields are %s", block, key, strings.Join(accepted, ", "))
		}
	}
	return nil
}

// ConfigMountRender is one resolved file mount, exactly as the Deployment
// renders it. Every field is already defaulted and validated.
type ConfigMountRender struct {
	VolumeName string
	// ConfigMap and Secret are mutually exclusive and name the projected
	// object; the template branches on which one is set.
	ConfigMap string
	Secret    string
	MountPath string
	// Mode is the octal literal written into the manifest (`defaultMode: 0440`)
	// and ModeValue the same mode as a number, so a test can assert the
	// rendered manifest parses back to the mode that was declared.
	Mode      string
	ModeValue uint32
	// ReadOnly is always true — resolve refuses anything else — and is carried
	// rather than hardcoded in the template so the one place that decides it is
	// also the one place that explains it, and so core's shared overlay is told
	// the same thing the manifest says.
	ReadOnly bool
	Optional bool
}

// ServiceAccountTokenRender is one resolved projected token, exactly as the
// Deployment renders it.
type ServiceAccountTokenRender struct {
	VolumeName        string
	Audience          string
	ExpirationSeconds int64
	// MountPath is the directory the projected volume is mounted at and
	// FileName the name the token is projected under, so MountPath/FileName is
	// the Path the service declared.
	MountPath string
	FileName  string
	// Mode is the octal literal written into the manifest. A token's mode is
	// not a service's choice: the file is written by the kubelet and read by
	// one process.
	Mode string
}

// Path is the in-container path of the token file: the value the service's own
// configuration points at.
func (t ServiceAccountTokenRender) Path() string { return path.Join(t.MountPath, t.FileName) }

// resolvePodMounts validates spec.config-mounts and spec.service-account-tokens
// together and returns what the Deployment renders.
//
// They resolve together because the invariants that matter span them: no two
// volumes may share a name or a container path, and neither may collide with a
// volume the template already renders. Validating each list alone would admit a
// pair that renders a pod the API server rejects — or, worse, one whose second
// mount silently shadows the first.
func (s *Settings) resolvePodMounts() ([]ConfigMountRender, []ServiceAccountTokenRender, error) {
	volumes := make(map[string]bool, len(s.ConfigMounts)+len(s.ServiceAccountTokens))
	paths := make(map[string]bool, len(s.ConfigMounts)+len(s.ServiceAccountTokens))

	claim := func(kind, volume, mountPath string) error {
		if reserved, taken := reservedVolumes[volume]; taken {
			return fmt.Errorf("%s volume name %q is reserved: the deployment already mounts it at %s", kind, volume, reserved)
		}
		for name, reserved := range reservedVolumes {
			if reserved == mountPath {
				return fmt.Errorf("%s path %q is already mounted by the deployment's %q volume", kind, mountPath, name)
			}
		}
		if volumes[volume] {
			return fmt.Errorf("%s volume name %q is declared more than once", kind, volume)
		}
		if paths[mountPath] {
			return fmt.Errorf("%s path %q is mounted more than once", kind, mountPath)
		}
		volumes[volume] = true
		paths[mountPath] = true
		return nil
	}

	var mounts []ConfigMountRender
	for _, mount := range s.ConfigMounts {
		resolved, err := mount.resolve()
		if err != nil {
			return nil, nil, err
		}
		if err := claim("config-mounts", resolved.VolumeName, resolved.MountPath); err != nil {
			return nil, nil, err
		}
		mounts = append(mounts, resolved)
	}

	var tokens []ServiceAccountTokenRender
	for _, token := range s.ServiceAccountTokens {
		resolved, err := token.resolve()
		if err != nil {
			return nil, nil, err
		}
		if err := claim("service-account-tokens", resolved.VolumeName, resolved.MountPath); err != nil {
			return nil, nil, err
		}
		tokens = append(tokens, resolved)
	}

	// A projected token carries the identity of the pod's ServiceAccount. With
	// no spec.service-account the pod runs under the namespace default, so the
	// token's subject is system:serviceaccount:<namespace>:default — a subject
	// the consumer's role does not bind. That failure surfaces as a refused
	// login long after a clean deploy, so refuse the pair here instead.
	if len(tokens) > 0 && s.ServiceAccount == nil {
		return nil, nil, fmt.Errorf(
			"service-account-tokens requires spec.service-account: a projected token carries the pod's ServiceAccount identity, and with none declared it is minted for the namespace default",
		)
	}
	return mounts, tokens, nil
}

// resolve defaults and validates one config mount.
func (m ConfigMount) resolve() (ConfigMountRender, error) {
	if (m.ConfigMap == "") == (m.Secret == "") {
		return ConfigMountRender{}, fmt.Errorf(
			"config-mounts entry for %q must name exactly one of config-map or secret",
			m.MountPath,
		)
	}
	source, kind := m.ConfigMap, "config-map"
	if m.Secret != "" {
		source, kind = m.Secret, "secret"
	}
	if len(source) > 253 || !dns1123Subdomain.MatchString(source) {
		return ConfigMountRender{}, fmt.Errorf("config-mounts %s %q must be a DNS-1123 subdomain", kind, source)
	}

	mountPath, err := containerDirectory(source, m.MountPath)
	if err != nil {
		return ConfigMountRender{}, err
	}

	volume := m.Name
	if volume == "" {
		// A ConfigMap or Secret name is a DNS-1123 subdomain and a volume name
		// a label, so the dots a subdomain allows have to go.
		volume = strings.ReplaceAll(source, ".", "-")
	}
	if len(volume) > 63 || !dns1123Label.MatchString(volume) {
		return ConfigMountRender{}, fmt.Errorf(
			"config-mounts volume name %q must be a DNS-1123 label (lowercase alphanumeric and '-'); declare `name` when the %s name is not one",
			volume, kind,
		)
	}

	if m.ReadOnly != nil && !*m.ReadOnly {
		return ConfigMountRender{}, fmt.Errorf(
			"config-mounts %q cannot set read-only: false: the container runs with a read-only root filesystem, and a writable configuration mount lets the process rewrite what it re-reads",
			mountPath,
		)
	}

	mode := m.Mode
	if mode == "" {
		mode = defaultFileMode
	}
	value, err := validateFileMode(mountPath, mode)
	if err != nil {
		return ConfigMountRender{}, err
	}

	return ConfigMountRender{
		VolumeName: volume,
		ConfigMap:  m.ConfigMap,
		Secret:     m.Secret,
		MountPath:  mountPath,
		Mode:       mode,
		ModeValue:  value,
		ReadOnly:   true,
		Optional:   m.Optional,
	}, nil
}

// resolve defaults and validates one projected token declaration.
func (t ServiceAccountToken) resolve() (ServiceAccountTokenRender, error) {
	audience := strings.TrimSpace(t.Audience)
	if audience == "" {
		return ServiceAccountTokenRender{}, fmt.Errorf(
			"service-account-tokens entry for %q requires an audience: an omitted audience mints the pod's API-server token, which is exactly the credential a projected token exists to avoid",
			t.Path,
		)
	}
	if audience != t.Audience {
		return ServiceAccountTokenRender{}, fmt.Errorf(
			"service-account-tokens audience %q must not be padded with whitespace: the consumer compares it literally",
			t.Audience,
		)
	}

	if !path.IsAbs(t.Path) || t.Path != path.Clean(t.Path) {
		return ServiceAccountTokenRender{}, fmt.Errorf(
			"service-account-tokens path %q must be an absolute, cleaned path to the token file (for example /var/run/secrets/vault/token)",
			t.Path,
		)
	}
	directory, fileName := path.Split(t.Path)
	mountPath, err := containerDirectory(audience, path.Clean(directory))
	if err != nil {
		return ServiceAccountTokenRender{}, err
	}
	if fileName == "" {
		return ServiceAccountTokenRender{}, fmt.Errorf(
			"service-account-tokens path %q must name the token file, not just a directory",
			t.Path,
		)
	}

	volume := t.Name
	if volume == "" {
		// An audience is free text and routinely a URL, which is no volume
		// name; derive one where we can and ask for `name` where we cannot.
		volume = strings.Trim(nonLabelCharacters.ReplaceAllString(strings.ToLower(audience), "-"), "-") + "-token"
	}
	if len(volume) > 63 || !dns1123Label.MatchString(volume) {
		return ServiceAccountTokenRender{}, fmt.Errorf(
			"service-account-tokens volume name %q must be a DNS-1123 label (lowercase alphanumeric and '-'); declare `name` when the audience does not yield one",
			volume,
		)
	}

	expiration := t.ExpirationSeconds
	if expiration == 0 {
		expiration = defaultTokenExpirationSeconds
	}
	if expiration < minTokenExpirationSeconds {
		return ServiceAccountTokenRender{}, fmt.Errorf(
			"service-account-tokens expiration-seconds %d for audience %q is below the %d Kubernetes requires, so the pod would be rejected at apply time; the kubelet already rotates the token at 80%% of its lifetime",
			expiration, audience, minTokenExpirationSeconds,
		)
	}
	if expiration > maxTokenExpirationSeconds {
		return ServiceAccountTokenRender{}, fmt.Errorf(
			"service-account-tokens expiration-seconds %d for audience %q is above the %d ceiling: a projected token is a bearer credential on disk, and a long lifetime widens the window a leaked copy stays valid",
			expiration, audience, maxTokenExpirationSeconds,
		)
	}

	return ServiceAccountTokenRender{
		VolumeName:        volume,
		Audience:          audience,
		ExpirationSeconds: expiration,
		MountPath:         mountPath,
		FileName:          fileName,
		Mode:              defaultFileMode,
	}, nil
}

// containerDirectory checks one absolute container directory a volume mounts at.
func containerDirectory(subject, mountPath string) (string, error) {
	if !path.IsAbs(mountPath) || mountPath != path.Clean(mountPath) || mountPath == "/" {
		return "", fmt.Errorf(
			"mount path %q for %q must be an absolute, cleaned container directory below the root",
			mountPath, subject,
		)
	}
	return mountPath, nil
}

// validateFileMode parses a declared file mode and refuses the modes that
// cannot work and the ones that should not be used.
//
// The container runs as uid/gid 65534 with fsGroup 65534, and the kubelet owns
// a projected volume's files root:fsGroup — so GROUP READ is what makes a
// mounted file readable at all, and a mode without it yields a pod that boots
// and then cannot read its own configuration. Group write, any world bit and
// any execute bit are refused: these are configuration files one non-root
// process reads, which leaves 0440 and 0640.
func validateFileMode(subject, mode string) (uint32, error) {
	refuse := func(reason string) (uint32, error) {
		return 0, fmt.Errorf(
			"file mode %q for %q %s; write it as a quoted octal string such as %q",
			mode, subject, reason, defaultFileMode,
		)
	}
	if !octalFileMode.MatchString(mode) {
		return refuse("is not a three-digit octal file mode with a leading 0")
	}
	parsed, err := strconv.ParseUint(mode, 0, 32)
	if err != nil {
		return refuse("is not a valid octal file mode")
	}
	value := uint32(parsed)
	switch {
	case value&0o007 != 0:
		return refuse("grants world access to a file only this pod reads")
	case value&0o020 != 0:
		return refuse("is group-writable, so anything sharing the pod's group could rewrite what the process re-reads")
	case value&0o111 != 0:
		return refuse("sets an execute bit on a configuration file")
	case value&0o400 == 0:
		return refuse("is not readable by its owner")
	case value&0o040 == 0:
		return refuse("is not group-readable, and the kubelet owns a mounted file root:fsGroup — so the container's own user could not read it")
	}
	return value, nil
}
