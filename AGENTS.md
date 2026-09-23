# Working in codefly-dev/service-go-grpc

`github.com/codefly-dev/service-go-grpc` (Go 1.27) is the codefly **service
agent** for Go services whose API is a protobuf contract. It is a plugin binary
served over gRPC, not a CLI: `codefly` starts it and drives its Builder and
Runtime RPCs. It owns the scaffolding a go-grpc service is generated from, the
proto→Go sync pipeline, the Docker build *recipe*, and the deployment manifests.

It does **not** own:

- **`codefly-dev/core`** — the resource model, the agent contracts, the proto
  source of truth, network and configuration. Contracts are consumed here and
  changed there.
- **`codefly-dev/service-go`** — generic Go service behaviour (settings, runner
  environments, `Code`, `Tooling`). This repo embeds it and adds gRPC / REST /
  Connect on top; `Init`, `Update`, `Test`, `Lint` are inherited untouched. A
  defect in generic Go behaviour is fixed there, not shadowed here.
- **`codefly-dev/cli`** — orchestration, and image building: `Build` emits a
  recipe, the CLI runs `docker buildx` and pushes.
- **the user's service** — everything outside the agent-owned scaffold
  (`rpcs.go`, `*.proto`, `go.mod`, business code) is theirs, and sync never
  touches it.

## How to behave

Fleet standard — [handbook#68](https://github.com/obin-ai/handbook/issues/68).
These bite here because this agent sits between the CLI and a service it
generates: a defect is almost always observed in one repo and owned by another.

- **A gap in the tooling is a bug in the tooling — never a reason to reach
  around it.** When a step `codefly`, `buf`, or the proto companion does not
  perform, the deliverable is that capability, named in the PR. Never a
  hand-assembled substitute — not as a "workaround", not "just this once", not
  "until the capability lands".
- **Never hack. Provide the best fix, even when it spans repos.** The fix living
  in `core`, `service-go`, or `cli` is not a reason to work around it here. Open
  the PR there and consume the reviewed result. When it genuinely cannot be
  fixed now, the deliverable is a precise issue against that owner plus an
  explicitly labelled stopgap — never an unlabelled one.
- **Classify every change that makes something work**, in the PR body: a *fix*
  at the place that owns the behaviour, or a *hack*. A hack does not become a
  fix by working, by being small, by being local, or by the real fix belonging
  elsewhere.
- **Never hardcode what the system resolves** — injected environment, derived
  ports, service addresses, credentials copied out of another component. This
  agent is on the *resolving* side: env vars and network mappings are built in
  `Runtime.Init`, ports come from the endpoints, images from the pinned
  constants. Typing one encodes something true on one machine for ten minutes,
  and it fails quietly — a runtime missing a credential can skip registration
  *silently*, so the service boots, serves, and is simply absent.
- **Diagnose, do not pattern-match.** "It started working when I set X" is not a
  diagnosis — set X back and confirm it breaks. Do not trust an error message
  before checking its claim: a buf failure reported as a permission error has
  meant an output path escaping the stage, not a broken container.
- **Say what you did not verify.** Unverified is not the same as working. Most
  of this suite skips when Docker or a backend is missing, so a green local run
  is not the tagged suite; if you could not exercise a path, the PR says so.

## Build and test

Derived from `.github/workflows/ci.yml`: a `dependency-lock` gate, then
`codefly-dev/core/.github/workflows/go-service-ci.yml@main`.

```bash
go test ./... -count=1 -run '^Test(FactoryDependencyLocksMatchBase|BaseGeneratedServiceBuildsFromCleanModuleCache)$'
go build ./...        # CI: Build
go vet ./...          # CI: Vet
go mod tidy -diff     # CI: fails if go.mod/go.sum drifted
go test ./...         # CI: go test -v ./...
```

Everything is one `package main`. CI takes the toolchain from `go.mod` and
deliberately **disables setup-go's cache** — it corrupts the downloaded
toolchain, and generated-service builds then fail with `package … is not in
std`. Releases are the `v*` tag driving GoReleaser, which builds with
`CGO_ENABLED=1` for darwin and linux.

**Never mock.** Fixtures are written to `t.TempDir()`, templates render through
core's real engine, and where the wire surface is what is under test the suite
starts a live in-process agent and talks to it through the generated client. So:
`TestBaseGeneratedServiceBuildsFromCleanModuleCache` needs network (it compiles
`base/code` against an empty module cache); `TestCreateToRun*` and
`TestGoGrpcLifecycle_Matrix` need Docker; `TestPackagingImage` is the one test
that builds an image and is opt-in via `CODEFLY_PACKAGING_IMAGE_TEST=1`. Around
90 s warm with backends present, several minutes cold.

Name a test for the invariant it holds, and give it a comment explaining why the
bug it prevents is possible. The existing tests do; match them.

## Where things live

| Path | Owns |
| --- | --- |
| `main.go` | agent registration, `Settings`, pinned Go/Alpine/runtime images, settings validation |
| `builder.go` | Builder RPCs — `Create`, `Sync`, `Build`, `Deploy`, `Upgrade`, endpoint discovery |
| `runtime.go` | Runtime RPCs — `Load`/`Init`/`Start`/`Stop`/`Destroy`, runner supervision, hot reload |
| `sync_transaction.go` | the staged, atomic apply that sync writes through |
| `sync_buf_output.go` | redirects `buf.gen.yaml` outputs that resolve outside the stage |
| `build_commands.go`, `runtime_asset_sources.go` | validation of what the recipe is allowed to stage |
| `commands.go`, `techniques.go` | agent commands (`proto`, `health`, `grpcurl…`) and prompts shipped **to the user's** agent — not guidance for this repo |
| `templates/factory` | what `Create` renders into a new service |
| `templates/builder`, `templates/deployment` | Dockerfile recipe; kustomize base + environment overlay |
| `base/` | a real, compiling generated service — the canonical fixture the factory templates must render to |

## Rules that bite

- **`base/` is a lock, not a sample.** `templates/factory/**` must render
  byte-for-byte to `base/**` — `go.mod`, `go.sum`, `grpc_gen.go`, the CORS
  adapter — and `base/code/go.mod`'s `core` line must equal this module's. Edit
  `base/code`, then mirror into the `.tmpl`s in the same commit. Dependabot does
  not watch `base/code`; that mirror is manual.
- **This agent never builds an image.** `Build` renders `templates/builder` into
  the caller's output directory and returns a `DockerBuildPlan`. A test that
  shells out to `docker build` means you are solving it in the wrong repo.
- **Private Go modules are the CLI's credential, never the recipe's.** The
  Dockerfile declares `ARG GOPRIVATE` and mounts the optional BuildKit secret
  `netrc` at `/root/.netrc` on every `go mod download`; the CLI supplies both
  (`GOPRIVATE` from the host, the secret from `CODEFLY_BUILD_NETRC`, `NETRC` or
  `~/.netrc`). Never add a credential as an `ARG`, `ENV` or `COPY` — that bakes
  a token into a layer — and keep the mount optional so the vendored recipe
  builds unchanged without one. `TestDockerfileTemplateFetchesPrivateModulesThroughAnOptionalSecret`
  holds the contract.
- **A manifest template interpolates a value, never pastes it.** The overlay
  ConfigMap emits every value as `{{ printf "%q" $value }}`: a configuration
  value is arbitrary text — a JSON document is the ordinary case — and a raw
  value between two literal quotes breaks the document the first time it holds a
  quote, a backslash or a newline, which surfaces as a `gitops render` failure
  in a consumer, not here. Go's `%q` escaping is a valid YAML double-quoted
  scalar and leaves a plain value byte-identical. The sibling Secret template
  keeps its literal quotes only because core base64-encodes `SecretMap`
  (`EnvsAsSecretData`); `TestSecretTemplateValuesAreBase64` pins that reason, and
  `TestConfigMapEscapesHostileValues` pins the escaping.
- **Sync is a transaction over a stage.** Copy in, generate, fix up, then swap
  by rename with rollback. Never write into a user's tree directly — a
  half-applied sync is a corrupted service. Buf's generation-input cache stays
  inside the transaction: a persistent input-only cache accepts discarded or
  tampered output on the next sync. Every sync regenerates before comparing.
- **Only the marked scaffold is agent-owned.** Sync overwrites `main.go`, the
  `*_gen.go` adapters and the plugin registry, and only when the generated
  marker and the single-service proto shape both hold. If generated output is
  wrong, fix the template — never the output.
- **Fail loud on a bad setting.** Conflicting CORS, health, or handler fields
  error rather than defaulting, and settings are re-unmarshalled into the
  go-grpc `Settings` on both `Load` paths because the inline generic type
  silently drops `rest-endpoint` / `connect-endpoint`. A silently dropped
  setting is worse than a failure.
- **Version moves as one change.** The embedded `agent.codefly.yaml` version,
  the `core` pin in both `go.mod`s, and the release tag belong in the same PR.

## Workflow

- Branch and PR; never commit to `main`. Conventional Commits for the title.
- CodeRabbit does not review automatically; ask for one with a PR comment.
- Keep this file under ~150 lines (hard cap 200). Push depth into a nested
  `AGENTS.md` beside what it describes, or into `.claude/skills/`.
- If a `CLAUDE.md` is ever added, make it a pointer to this file. One canonical
  source.
- Treat this file as code: the PR that changes a process updates it.
