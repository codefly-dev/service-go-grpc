package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	agenttesting "github.com/codefly-dev/core/agents/testing"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8syaml "sigs.k8s.io/yaml"
)

// vaultBindingSettings is the declaration the worked example in
// docs/deployment-mounts.md describes, and the one
// codefly-dev/module-saas-starter#1008 renders accounts with: the cell's Vault
// CA on disk, and a token minted for Vault rather than for the API server.
func vaultBindingSettings() *Settings {
	return &Settings{
		ServiceAccount: &ServiceAccountSpec{Name: "accounts"},
		ConfigMounts: []ConfigMount{{
			ConfigMap: "vault-ca",
			MountPath: "/etc/vault/tls",
		}},
		ServiceAccountTokens: []ServiceAccountToken{{
			Audience:          "vault",
			Path:              "/var/run/secrets/vault/token",
			ExpirationSeconds: 600,
		}},
	}
}

// renderPodSpec renders the Deployment through every output profile core
// supports — the restricted ones included, which is where the volume-source
// allowlist applies — and parses the result with the same library Kubernetes
// uses, so a manifest that would not apply is a failure here.
func renderPodSpec(t *testing.T, settings *Settings) (corev1.PodSpec, string) {
	t.Helper()
	mounts, tokens, err := settings.resolvePodMounts()
	if err != nil {
		t.Fatalf("resolve pod mounts: %v", err)
	}
	dir := agenttesting.AssertKustomizeTemplatesWithOverlay(t, deploymentFS, DeploymentParameters{
		ServiceAccount:       settings.ServiceAccount,
		Health:               undeclaredHealth(),
		ConfigMounts:         mounts,
		ServiceAccountTokens: tokens,
	}, podOverlay(mounts))
	rendered, err := os.ReadFile(filepath.Join(dir, "base", "deployment.yaml"))
	if err != nil {
		t.Fatalf("read deployment: %v", err)
	}
	var deployment appsv1.Deployment
	if err := k8syaml.UnmarshalStrict(rendered, &deployment); err != nil {
		t.Fatalf("rendered deployment is not a valid Deployment: %v\n%s", err, rendered)
	}
	return deployment.Spec.Template.Spec, string(rendered)
}

func volumeNamed(pod corev1.PodSpec, name string) (corev1.Volume, bool) {
	for _, volume := range pod.Volumes {
		if volume.Name == name {
			return volume, true
		}
	}
	return corev1.Volume{}, false
}

func mountNamed(pod corev1.PodSpec, name string) (corev1.VolumeMount, bool) {
	for _, mount := range pod.Containers[0].VolumeMounts {
		if mount.Name == name {
			return mount, true
		}
	}
	return corev1.VolumeMount{}, false
}

// TestDeploymentRendersDeclaredMounts is the end-to-end shape of this feature:
// a service declaring one config mount and one projected token renders a pod
// carrying both volumes and both mounts, under every profile including the
// restricted ones.
//
// It asserts the mode as a NUMBER read back out of the manifest, because the
// template writes the octal literal `defaultMode: 0440` and that is only the
// mode we meant if whatever parses the manifest resolves it as octal. A
// manifest saying 440 decimal would mount the CA as 0670 and no other
// assertion here would notice.
func TestDeploymentRendersDeclaredMounts(t *testing.T) {
	pod, rendered := renderPodSpec(t, vaultBindingSettings())

	volume, found := volumeNamed(pod, "vault-ca")
	if !found {
		t.Fatalf("no vault-ca volume rendered:\n%s", rendered)
	}
	if volume.ConfigMap == nil {
		t.Fatalf("vault-ca volume is not backed by a ConfigMap:\n%s", rendered)
	}
	if volume.ConfigMap.Name != "vault-ca" {
		t.Errorf("vault-ca volume names ConfigMap %q, want vault-ca", volume.ConfigMap.Name)
	}
	if volume.ConfigMap.DefaultMode == nil || *volume.ConfigMap.DefaultMode != 0o440 {
		t.Errorf("vault-ca defaultMode = %v, want 0440 (288 decimal):\n%s", volume.ConfigMap.DefaultMode, rendered)
	}
	if volume.ConfigMap.Optional == nil || *volume.ConfigMap.Optional {
		t.Errorf("vault-ca must not be optional by default: %v", volume.ConfigMap.Optional)
	}
	mount, found := mountNamed(pod, "vault-ca")
	if !found {
		t.Fatalf("no vault-ca volumeMount rendered:\n%s", rendered)
	}
	if mount.MountPath != "/etc/vault/tls" || !mount.ReadOnly {
		t.Errorf("vault-ca mount = %+v, want /etc/vault/tls read-only", mount)
	}
	if mount.SubPath != "" {
		t.Errorf("vault-ca mount uses subPath %q: a subPath mount is never refreshed when its ConfigMap changes", mount.SubPath)
	}

	token, found := volumeNamed(pod, "vault-token")
	if !found {
		t.Fatalf("no vault-token volume rendered:\n%s", rendered)
	}
	if token.Projected == nil || len(token.Projected.Sources) != 1 {
		t.Fatalf("vault-token is not a projected volume with one source:\n%s", rendered)
	}
	source := token.Projected.Sources[0].ServiceAccountToken
	if source == nil {
		t.Fatalf("vault-token source is not a serviceAccountToken:\n%s", rendered)
	}
	if source.Audience != "vault" {
		t.Errorf("token audience = %q, want vault", source.Audience)
	}
	if source.ExpirationSeconds == nil || *source.ExpirationSeconds != 600 {
		t.Errorf("token expirationSeconds = %v, want 600", source.ExpirationSeconds)
	}
	if source.Path != "token" {
		t.Errorf("token projected path = %q, want token", source.Path)
	}
	if token.Projected.DefaultMode == nil || *token.Projected.DefaultMode != 0o440 {
		t.Errorf("vault-token defaultMode = %v, want 0440:\n%s", token.Projected.DefaultMode, rendered)
	}
	tokenMount, found := mountNamed(pod, "vault-token")
	if !found {
		t.Fatalf("no vault-token volumeMount rendered:\n%s", rendered)
	}
	// The service configures a FILE path; the volume is mounted at its
	// directory and the token projected under its base name, so the two
	// together have to reproduce exactly the path that was declared.
	if got := filepath.Join(tokenMount.MountPath, source.Path); got != "/var/run/secrets/vault/token" {
		t.Errorf("token lands at %q, want the declared /var/run/secrets/vault/token", got)
	}
	if !tokenMount.ReadOnly {
		t.Errorf("token mount must be read-only: %+v", tokenMount)
	}

	// The whole point of a declared audience: the pod still never carries the
	// API server's own token.
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Errorf("automountServiceAccountToken = %v, want false even with a projected token:\n%s", pod.AutomountServiceAccountToken, rendered)
	}
	if pod.ServiceAccountName != "accounts" {
		t.Errorf("serviceAccountName = %q, want accounts — a projected token carries that identity", pod.ServiceAccountName)
	}
}

// TestDeploymentRendersASecretBackedMount covers the other source. A Secret is
// mounted the same way and read the same way; only the volume source differs.
func TestDeploymentRendersASecretBackedMount(t *testing.T) {
	pod, rendered := renderPodSpec(t, &Settings{
		ConfigMounts: []ConfigMount{{
			Name:      "client-cert",
			Secret:    "accounts-client-tls",
			MountPath: "/etc/accounts/client",
			Mode:      "0640",
			Optional:  true,
		}},
	})
	volume, found := volumeNamed(pod, "client-cert")
	if !found {
		t.Fatalf("no client-cert volume rendered:\n%s", rendered)
	}
	if volume.Secret == nil {
		t.Fatalf("client-cert volume is not backed by a Secret:\n%s", rendered)
	}
	if volume.Secret.SecretName != "accounts-client-tls" {
		t.Errorf("client-cert names Secret %q", volume.Secret.SecretName)
	}
	if volume.Secret.Optional == nil || !*volume.Secret.Optional {
		t.Errorf("declared optional was dropped: %v", volume.Secret.Optional)
	}
	if volume.Secret.DefaultMode == nil || *volume.Secret.DefaultMode != 0o640 {
		t.Errorf("client-cert defaultMode = %v, want 0640", volume.Secret.DefaultMode)
	}
}

// TestDeploymentWithoutMountsIsUnchanged pins that this feature is inert when
// nothing declares it: the pod keeps exactly the one scratch volume it has
// always had. Every service that already deploys renders through this path, so
// an extra volume here would change every one of them.
func TestDeploymentWithoutMountsIsUnchanged(t *testing.T) {
	for name, settings := range map[string]*Settings{
		"nothing declared": {},
		"empty lists":      {ConfigMounts: []ConfigMount{}, ServiceAccountTokens: []ServiceAccountToken{}},
	} {
		t.Run(name, func(t *testing.T) {
			pod, rendered := renderPodSpec(t, settings)
			if len(pod.Volumes) != 1 || pod.Volumes[0].Name != "tmp" || pod.Volumes[0].EmptyDir == nil {
				t.Errorf("want exactly the tmp emptyDir volume, got %+v:\n%s", pod.Volumes, rendered)
			}
			if mounts := pod.Containers[0].VolumeMounts; len(mounts) != 1 || mounts[0].MountPath != "/tmp" {
				t.Errorf("want exactly the /tmp mount, got %+v", mounts)
			}
			if strings.Contains(rendered, "projected:") {
				t.Errorf("no projected volume should render:\n%s", rendered)
			}
		})
	}
}

// TestPodOverlayDeclaresConfigMapMountsToCore pins the seam this agent shares
// with every other: a ConfigMap-backed mount is declared on core's pod overlay,
// with the SAME volume name the template renders. Core warns when an overlay
// asks for a mount the rendered workload carries no volume for, and that check
// is keyed on the volume name — derive it in two places and the check silently
// passes against the wrong name, or silently fails against the right one.
func TestPodOverlayDeclaresConfigMapMountsToCore(t *testing.T) {
	settings := vaultBindingSettings()
	settings.ConfigMounts = append(settings.ConfigMounts, ConfigMount{
		Secret:    "accounts-client-tls",
		MountPath: "/etc/accounts/client",
	})
	mounts, _, err := settings.resolvePodMounts()
	if err != nil {
		t.Fatalf("resolve pod mounts: %v", err)
	}
	overlay := podOverlay(mounts)
	if !overlay.HasConfigMounts() {
		t.Fatalf("overlay declares no config mounts")
	}
	// Only the ConfigMap-backed one: services.ConfigMount models a ConfigMap
	// source and nothing else, so declaring the Secret mount there would have
	// core validate an object that does not exist.
	if len(overlay.ConfigMounts) != 1 {
		t.Fatalf("overlay declares %d mounts, want only the ConfigMap-backed one: %+v", len(overlay.ConfigMounts), overlay.ConfigMounts)
	}
	declared := overlay.ConfigMounts[0]
	if declared.ConfigMapName != "vault-ca" || declared.MountPath != "/etc/vault/tls" {
		t.Errorf("overlay mount = %+v, want the declared vault-ca mount", declared)
	}
	if declared.ReadOnly == nil || !*declared.ReadOnly {
		t.Errorf("overlay mount is not read-only: %v", declared.ReadOnly)
	}
	if err := overlay.Validate(); err != nil {
		t.Errorf("core refuses the overlay this agent declares: %v", err)
	}

	pod, rendered := renderPodSpec(t, settings)
	if _, found := volumeNamed(pod, declared.VolumeName); !found {
		t.Errorf("overlay names volume %q but the template rendered none:\n%s", declared.VolumeName, rendered)
	}
}

// TestManifestValidatorRefusesAHostPathVolume proves the volume-source
// allowlist this feature relies on is a real guard and not a comment. The
// template cannot render a hostPath — a mount names a config-map or a secret —
// so the rendered manifest is altered and handed back to the same validator
// every profile run goes through.
//
// The probe runs on the ephemeral render because that is the tree the helper
// returns; core applies restrictedVolumeSources in validatePodSpec for every
// supported profile, restricted and ephemeral alike, so the allowlist under
// test here is the same one a restricted render is held to.
func TestManifestValidatorRefusesAHostPathVolume(t *testing.T) {
	mounts, tokens, err := vaultBindingSettings().resolvePodMounts()
	if err != nil {
		t.Fatalf("resolve pod mounts: %v", err)
	}
	dir := agenttesting.AssertKustomizeTemplatesWithOverlay(t, deploymentFS, DeploymentParameters{
		ServiceAccount:       &ServiceAccountSpec{Name: "accounts"},
		Health:               undeclaredHealth(),
		ConfigMounts:         mounts,
		ServiceAccountTokens: tokens,
	}, podOverlay(mounts))

	profile := builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1
	path := filepath.Join(dir, "base", "deployment.yaml")
	manifest, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read deployment: %v", err)
	}
	if before := services.ValidateKubernetesManifestTree(context.Background(), dir, "test", "codefly-test", profile, false, "", ""); before.GetStaticValidation() != builderv0.KubernetesManifestValidation_STATUS_PASSED {
		t.Fatalf("the rendered tree must pass before it is altered:\n%s", strings.Join(before.GetViolations(), "\n"))
	}

	altered := strings.Replace(string(manifest), "        - name: tmp\n          emptyDir: {}", "        - name: tmp\n          hostPath:\n            path: /var/lib/kubelet", 1)
	if altered == string(manifest) {
		t.Fatal("could not alter the rendered tmp volume; the template changed shape")
	}
	if err := os.WriteFile(path, []byte(altered), 0o600); err != nil {
		t.Fatalf("write altered deployment: %v", err)
	}
	validation := services.ValidateKubernetesManifestTree(context.Background(), dir, "test", "codefly-test", profile, false, "", "")
	if validation.GetStaticValidation() == builderv0.KubernetesManifestValidation_STATUS_PASSED {
		t.Fatal("a hostPath volume passed the manifest validator")
	}
	if !strings.Contains(strings.Join(validation.GetViolations(), "\n"), "hostPath") {
		t.Errorf("the refusal does not name hostPath: %v", validation.GetViolations())
	}
}

// TestResolvePodMountsRefusesUnsafeDeclarations is the validator's contract.
// Each case is a declaration that renders a pod which either cannot apply, or
// applies and then fails somewhere far from the manifest — a mount the process
// can rewrite, a file every other process on the node can read, a token the
// API server minted for itself. Each must be refused by name, here.
func TestResolvePodMountsRefusesUnsafeDeclarations(t *testing.T) {
	readWrite := false
	readOnly := true
	for name, tc := range map[string]struct {
		settings *Settings
		want     string
	}{
		"writable mount": {
			settings: &Settings{ConfigMounts: []ConfigMount{{
				ConfigMap: "vault-ca", MountPath: "/etc/vault/tls", ReadOnly: &readWrite,
			}}},
			want: "read-only: false",
		},
		"explicit read-only is accepted": {
			settings: &Settings{ConfigMounts: []ConfigMount{{
				ConfigMap: "vault-ca", MountPath: "/etc/vault/tls", ReadOnly: &readOnly,
			}}},
		},
		"world-writable mode": {
			settings: &Settings{ConfigMounts: []ConfigMount{{
				ConfigMap: "vault-ca", MountPath: "/etc/vault/tls", Mode: "0446",
			}}},
			want: "world access",
		},
		"world-readable mode": {
			settings: &Settings{ConfigMounts: []ConfigMount{{
				ConfigMap: "vault-ca", MountPath: "/etc/vault/tls", Mode: "0444",
			}}},
			want: "world access",
		},
		"group-writable mode": {
			settings: &Settings{ConfigMounts: []ConfigMount{{
				ConfigMap: "vault-ca", MountPath: "/etc/vault/tls", Mode: "0460",
			}}},
			want: "group-writable",
		},
		"mode the container could not read": {
			settings: &Settings{ConfigMounts: []ConfigMount{{
				ConfigMap: "vault-ca", MountPath: "/etc/vault/tls", Mode: "0400",
			}}},
			want: "group-readable",
		},
		// An unquoted `mode: 0644` survives the spec's map round-trip as the
		// decimal text "420", which is a perfectly valid octal mode — 0420.
		// Requiring the leading zero is what stops that being applied.
		"decimal mode from an unquoted octal": {
			settings: &Settings{ConfigMounts: []ConfigMount{{
				ConfigMap: "vault-ca", MountPath: "/etc/vault/tls", Mode: "420",
			}}},
			want: "leading 0",
		},
		"neither source": {
			settings: &Settings{ConfigMounts: []ConfigMount{{MountPath: "/etc/vault/tls"}}},
			want:     "exactly one of config-map or secret",
		},
		"both sources": {
			settings: &Settings{ConfigMounts: []ConfigMount{{
				ConfigMap: "vault-ca", Secret: "vault-ca", MountPath: "/etc/vault/tls",
			}}},
			want: "exactly one of config-map or secret",
		},
		"relative mount path": {
			settings: &Settings{ConfigMounts: []ConfigMount{{ConfigMap: "vault-ca", MountPath: "etc/vault"}}},
			want:     "absolute",
		},
		"mount path escaping its directory": {
			settings: &Settings{ConfigMounts: []ConfigMount{{ConfigMap: "vault-ca", MountPath: "/etc/vault/../../root"}}},
			want:     "cleaned",
		},
		"mount shadowing the scratch volume": {
			settings: &Settings{ConfigMounts: []ConfigMount{{ConfigMap: "vault-ca", MountPath: "/tmp"}}},
			want:     "already mounted by the deployment",
		},
		"volume name colliding with the scratch volume": {
			settings: &Settings{ConfigMounts: []ConfigMount{{
				Name: "tmp", ConfigMap: "vault-ca", MountPath: "/etc/vault/tls",
			}}},
			want: "reserved",
		},
		"two mounts at one path": {
			settings: &Settings{ConfigMounts: []ConfigMount{
				{ConfigMap: "vault-ca", MountPath: "/etc/vault/tls"},
				{ConfigMap: "other-ca", MountPath: "/etc/vault/tls"},
			}},
			want: "mounted more than once",
		},
		"two mounts sharing a volume name": {
			settings: &Settings{ConfigMounts: []ConfigMount{
				{Name: "ca", ConfigMap: "vault-ca", MountPath: "/etc/vault/tls"},
				{Name: "ca", ConfigMap: "other-ca", MountPath: "/etc/other/tls"},
			}},
			want: "declared more than once",
		},
		"configmap name that is no volume name": {
			settings: &Settings{ConfigMounts: []ConfigMount{{
				ConfigMap: "Vault_CA", MountPath: "/etc/vault/tls",
			}}},
			want: "DNS-1123 subdomain",
		},
		"token without an audience": {
			settings: &Settings{
				ServiceAccount:       &ServiceAccountSpec{Name: "accounts"},
				ServiceAccountTokens: []ServiceAccountToken{{Path: "/var/run/secrets/vault/token"}},
			},
			want: "requires an audience",
		},
		"token with a padded audience": {
			settings: &Settings{
				ServiceAccount:       &ServiceAccountSpec{Name: "accounts"},
				ServiceAccountTokens: []ServiceAccountToken{{Audience: "vault ", Path: "/var/run/secrets/vault/token"}},
			},
			want: "whitespace",
		},
		// 300 s is what the consumer's Vault role issues, and it reads like a
		// natural thing to ask the kubelet for. Kubernetes rejects any
		// projected token below 600 s, so the pod would simply never apply.
		"token below the Kubernetes floor": {
			settings: &Settings{
				ServiceAccount: &ServiceAccountSpec{Name: "accounts"},
				ServiceAccountTokens: []ServiceAccountToken{{
					Audience: "vault", Path: "/var/run/secrets/vault/token", ExpirationSeconds: 300,
				}},
			},
			want: "below the 600 Kubernetes requires",
		},
		"token above the ceiling": {
			settings: &Settings{
				ServiceAccount: &ServiceAccountSpec{Name: "accounts"},
				ServiceAccountTokens: []ServiceAccountToken{{
					Audience: "vault", Path: "/var/run/secrets/vault/token", ExpirationSeconds: 86400,
				}},
			},
			want: "above the 3600 ceiling",
		},
		"token path that is a directory": {
			settings: &Settings{
				ServiceAccount:       &ServiceAccountSpec{Name: "accounts"},
				ServiceAccountTokens: []ServiceAccountToken{{Audience: "vault", Path: "/var/run/secrets/vault/"}},
			},
			want: "cleaned",
		},
		"token at the container root": {
			settings: &Settings{
				ServiceAccount:       &ServiceAccountSpec{Name: "accounts"},
				ServiceAccountTokens: []ServiceAccountToken{{Audience: "vault", Path: "/token"}},
			},
			want: "below the root",
		},
		// An audience is free text and routinely a URL, so the derived volume
		// name has to survive one. A URL audience that cannot be squeezed into
		// a 63-character label asks for `name` rather than rendering a pod the
		// API server rejects.
		"token whose audience yields no volume name": {
			settings: &Settings{
				ServiceAccount: &ServiceAccountSpec{Name: "accounts"},
				ServiceAccountTokens: []ServiceAccountToken{{
					Audience: "https://vault." + strings.Repeat("a", 64) + ".example.com/v1/",
					Path:     "/var/run/secrets/vault/token",
				}},
			},
			want: "declare `name`",
		},
		"token whose audience is named explicitly": {
			settings: &Settings{
				ServiceAccount: &ServiceAccountSpec{Name: "accounts"},
				ServiceAccountTokens: []ServiceAccountToken{{
					Name:     "vault-token",
					Audience: "https://vault." + strings.Repeat("a", 64) + ".example.com/v1/",
					Path:     "/var/run/secrets/vault/token",
				}},
			},
		},
		"token whose URL audience yields one": {
			settings: &Settings{
				ServiceAccount: &ServiceAccountSpec{Name: "accounts"},
				ServiceAccountTokens: []ServiceAccountToken{{
					Audience: "https://vault.example.com/v1/", Path: "/var/run/secrets/vault/token",
				}},
			},
		},
		"token without a service account": {
			settings: &Settings{ServiceAccountTokens: []ServiceAccountToken{{
				Audience: "vault", Path: "/var/run/secrets/vault/token",
			}}},
			want: "requires spec.service-account",
		},
		"token colliding with a config mount": {
			settings: &Settings{
				ServiceAccount: &ServiceAccountSpec{Name: "accounts"},
				ConfigMounts: []ConfigMount{{
					Name: "vault-token", ConfigMap: "vault-ca", MountPath: "/etc/vault/tls",
				}},
				ServiceAccountTokens: []ServiceAccountToken{{
					Audience: "vault", Path: "/var/run/secrets/vault/token",
				}},
			},
			want: "declared more than once",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := tc.settings.resolvePodMounts()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want a refusal naming %q, got none", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not name %q", err, tc.want)
			}
		})
	}
}

// TestSettingsValidateCoversPodMounts pins that the refusals above reach the
// same place every other bad setting does. Deploy validates settings before it
// resolves anything, so a mount that only resolvePodMounts refused would pass
// `codefly sync` and fail at deploy.
func TestSettingsValidateCoversPodMounts(t *testing.T) {
	settings := &Settings{ConfigMounts: []ConfigMount{{ConfigMap: "vault-ca", MountPath: "/tmp"}}}
	if err := settings.Validate(); err == nil {
		t.Fatal("Settings.Validate accepted a mount that shadows the scratch volume")
	}
}

// TestConfigMountRefusesAnUnknownField pins the decoding half. A mount is the
// one declaration here where a dropped key changes a security property in
// silence — `host-path:` is the case this feature must never grow by accident —
// so an unknown field is named rather than ignored.
func TestConfigMountRefusesAnUnknownField(t *testing.T) {
	for name, document := range map[string]string{
		"host path":           "config-mounts:\n  - host-path: /var/lib/kubelet\n    mount-path: /host\n",
		"misspelt read-only":  "config-mounts:\n  - config-map: vault-ca\n    mount-path: /etc/vault/tls\n    readonly: false\n",
		"misspelt mount path": "config-mounts:\n  - config-map: vault-ca\n    mountPath: /etc/vault/tls\n",
		"unknown token field": "service-account-tokens:\n  - audience: vault\n    path: /var/run/secrets/vault/token\n    bound-object: pod\n",
		"token audience typo": "service-account-tokens:\n  - audiences: vault\n    path: /var/run/secrets/vault/token\n",
	} {
		t.Run(name, func(t *testing.T) {
			var settings Settings
			err := yaml.Unmarshal([]byte(document), &settings)
			if err == nil {
				t.Fatalf("unknown field was accepted: %+v", settings)
			}
			if !strings.Contains(err.Error(), "unknown field") || !strings.Contains(err.Error(), "accepted fields are") {
				t.Errorf("refusal %q neither names the unknown field nor lists the accepted ones", err)
			}
		})
	}
}

// TestConfigMountAcceptsAQuotedOctalMode pins the spelling the documentation
// tells a service author to write, decoded the way a service spec reaches these
// settings (resources.Service.LoadSettingsFromSpec marshals the spec map and
// unmarshals it here).
func TestConfigMountAcceptsAQuotedOctalMode(t *testing.T) {
	for _, mode := range []string{`"0440"`, `"0o440"`} {
		var settings Settings
		document := "config-mounts:\n  - config-map: vault-ca\n    mount-path: /etc/vault/tls\n    mode: " + mode + "\n"
		if err := yaml.Unmarshal([]byte(document), &settings); err != nil {
			t.Fatalf("mode %s was refused at decode: %v", mode, err)
		}
		mounts, _, err := settings.resolvePodMounts()
		if err != nil {
			t.Fatalf("mode %s was refused: %v", mode, err)
		}
		if mounts[0].ModeValue != 0o440 {
			t.Errorf("mode %s resolved to %#o, want 0440", mode, mounts[0].ModeValue)
		}
	}
}

// TestDefaultFileModeIsTheModeItClaims guards the constant every mount and
// every projected token falls back to. It is a string the template pastes into
// a manifest, so a typo — "0040", "0444" — would render a pod that either
// cannot read its own configuration or publishes it to the node, and no other
// test here states the number.
func TestDefaultFileModeIsTheModeItClaims(t *testing.T) {
	value, err := validateFileMode("the default", defaultFileMode)
	if err != nil {
		t.Fatalf("the default mode %q is refused by its own validator: %v", defaultFileMode, err)
	}
	if value != 0o440 {
		t.Errorf("defaultFileMode %q resolves to %#o, want 0440", defaultFileMode, value)
	}
}
