# Mounting files into a deployed pod

A go-grpc service reaches its configuration through environment variables: the
deploy ConfigMap, projected into the container with `envFrom`. Two things cannot
travel that way.

- **A file the process must open.** A TLS client verifying a private CA needs a
  PEM at a path, not a variable holding one.
- **A credential the cluster mints, not the workspace.** A projected
  ServiceAccount token is written and rotated by the kubelet; nothing can hand
  it over as a value because it does not exist until the pod does.

So the manifest declares both, in `spec`, and the Deployment renders the volumes
and mounts. Everything is resolved and validated before it reaches the template:
a declaration the pod could not actually use is refused at `Validate` and
`Deploy`, not discovered when a consumer applies the manifest or when the
process first reads the file.

Nothing renders unless something is declared. A service that declares neither
gets the pod it has always had: one `/tmp` `emptyDir`, and
`automountServiceAccountToken: false`.

## `spec.config-mounts`

One entry projects a named ConfigMap or Secret into the pod as read-only files.

```yaml
spec:
  config-mounts:
    - config-map: vault-ca        # exactly one of config-map / secret
      mount-path: /etc/vault/tls  # absolute container DIRECTORY
      mode: "0440"                # optional, quoted octal, default "0440"
      optional: false             # optional, default false
      name: vault-ca              # optional, the pod volume name
```

| field | meaning |
| --- | --- |
| `config-map` / `secret` | the object to project. Exactly one. It is **named here, never created here**, so an environment supplies it out-of-band — a cell's workload-CA stage writes it, say. |
| `mount-path` | the absolute container **directory** the keys land in. A ConfigMap key `ca.crt` mounted at `/etc/vault/tls` is the file `/etc/vault/tls/ca.crt`. |
| `mode` | the projected files' permissions, as a **quoted** octal string. Defaults to `"0440"`. |
| `optional` | let the pod start when the object is absent. Defaults to `false`: a workload whose configuration has not arrived should wait visibly rather than boot without it. |
| `name` | the pod volume name, a DNS-1123 label. Derived from the object's name when omitted. |

`read-only` can only be set to `true`, and there is no `sub-path` at all:

- Every mount renders `readOnly: true`. The container runs with
  `readOnlyRootFilesystem`, and a writable configuration mount is a way for the
  process to rewrite what it later re-reads. `read-only: true` is accepted
  because declaring the default is reasonable; `read-only: false` is refused by
  name rather than quietly ignored.
- There is no `subPath`, because a `subPath` mount is never refreshed when its
  ConfigMap or Secret changes — the pod would keep serving the first value it
  ever saw, which is indistinguishable from a rotation that did not happen.

### Quote the mode

`mode: "0440"`, with the quotes. A service spec is read into a map and
re-marshalled on its way into these settings, so an **unquoted** `mode: 0644` is
resolved as the YAML 1.1 octal integer `420` and arrives as the text `"420"` —
which is itself a valid octal mode, `0420`. Requiring the leading zero makes
that round-trip a refusal instead of a mode you did not ask for.

### Which modes are accepted

`0440` and `0640`, and that is deliberate. The container runs as uid/gid 65534
with `fsGroup: 65534`, and the kubelet owns a projected volume's files
`root:fsGroup` — so **group read is what makes a mounted file readable at all**,
and a mode without it yields a pod that boots and then cannot read its own
configuration. Refused on the other side: any world bit (every other process on
the node could read it), group write (anything sharing the pod's group could
rewrite what the process re-reads), and any execute bit.

## `spec.service-account-tokens`

One entry projects a ServiceAccount token for a **declared audience** at a
declared path.

```yaml
spec:
  service-account-tokens:
    - audience: vault                        # mandatory
      path: /var/run/secrets/vault/token     # absolute path of the token FILE
      expiration-seconds: 600                # optional, default 600
      name: vault-token                      # optional, the pod volume name
```

| field | meaning |
| --- | --- |
| `audience` | the intended recipient, and **mandatory**. See below. |
| `path` | the absolute path of the token **file** — the value the process is configured with. The volume mounts at its directory and the token is projected under its base name. |
| `expiration-seconds` | the requested lifetime. Defaults to 600; must be at least 600 and at most 3600. |
| `name` | the pod volume name, a DNS-1123 label. Derived from the audience when omitted. |

**The audience is mandatory because an omitted one is not "no audience" — it is
the API server.** Kubernetes documents the field plainly: "The audience
defaults to the identifier of the apiserver". A consumer
such as Vault's Kubernetes auth method accepts only a token minted for its own
audience, so an undeclared audience produces a pod that deploys cleanly, starts,
and fails every login — the kind of failure that surfaces nowhere near the
manifest that caused it.

`automountServiceAccountToken` stays `false` whatever is declared here. These
projections are not the pod's API-server token and do not need it.

**The lifetime floor is Kubernetes', not this agent's.** A projected token below
600 seconds is rejected at apply time (`corev1.ServiceAccountTokenProjection`:
"Defaults to 1 hour and must be at least 10 minutes"), so asking for 300 renders
a Deployment that cannot be applied. If the consumer issues *its own* tokens
with a 300 s TTL — a Vault role's `token_ttl`, say — that is a different number
and belongs in the consumer's configuration, not here. The ceiling of 3600 is
this agent's: a projected token is a bearer credential on a pod's disk, the
kubelet rotates the file at 80% of its lifetime, so a short lifetime costs
nothing while a long one widens the window in which a leaked copy still
authenticates.

**A token needs `spec.service-account`.** The token carries the identity of the
pod's ServiceAccount, and with none declared the pod runs under the namespace
default — so the token's subject is `system:serviceaccount:<namespace>:default`,
which the consumer's role does not bind. Declaring a token without a
ServiceAccount is refused.

## Worked example: binding a cell's Vault with Kubernetes auth

This is the case the fields were added for
(codefly-dev/service-go-grpc#156, consumed by
codefly-dev/module-saas-starter#1008). A service authenticates to a Vault the
cell runs, over TLS, with no Vault token anywhere: it presents a projected
ServiceAccount token minted for Vault, and verifies Vault's certificate against
a CA the cell publishes as a ConfigMap.

```yaml
spec:
  service-account:
    name: accounts

  config-mounts:
    - config-map: vault-ca
      mount-path: /etc/vault/tls

  service-account-tokens:
    - audience: vault
      path: /var/run/secrets/vault/token
      expiration-seconds: 600
```

That renders a pod with the CA at `/etc/vault/tls/ca.crt` mode `0440` (the key
name inside the ConfigMap is the file name), a Vault-audience token at
`/var/run/secrets/vault/token`, `serviceAccountName: accounts`, and
`automountServiceAccountToken: false`.

The service's own configuration then points at those paths. Nothing here
hardcodes them on the service's behalf — the paths are declared once, in the
manifest, and the configuration group names the same ones:

| key | value |
| --- | --- |
| `VAULT_ADDR` | `https://vault.vault.svc.cluster.local:8200` |
| `VAULT_AUTH_METHOD` | `kubernetes` |
| `VAULT_CA_FILE` | `/etc/vault/tls/ca.crt` — the `mount-path` plus the ConfigMap's key |
| `VAULT_K8S_TOKEN_PATH` | `/var/run/secrets/vault/token` — the declared `path` |
| `VAULT_K8S_ROLE` | the Vault role bound to the `accounts` ServiceAccount |

Two things the cluster side owes, which no manifest field can supply: the
ConfigMap `vault-ca` must exist in the pod's namespace (it is named here, not
created here), and the Vault Kubernetes auth role must accept the audience
`vault` and bind that namespace's `accounts` ServiceAccount. A role that binds
the subject but not the audience refuses the login with the token the pod does
carry.
