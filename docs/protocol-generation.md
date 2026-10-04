# Protocol inputs during sync

Sync replaces the directories declared by `protocol-output-dirs`. Every contract
that generates into those directories must participate in that generation.
Use a Buf v2 generation template to declare multiple inputs or to generate only
selected paths while keeping imported descriptors available:

```yaml
version: v2
inputs:
  - directory: .
    paths:
      - api/v1/api.proto
  - directory: ../proto-client
plugins:
  - local: protoc-gen-go
    out: ../code/pkg/gen
    opt: paths=source_relative
```

Directory inputs resolve relative to `buf.gen.yaml`. Sync stages each declared
local directory alongside the primary protocol directory before running Buf.
Directories must stay below the service root; absolute paths and paths outside
that root fail before output is applied. The template retains its input paths,
options and plugins. Buf owns their interpretation and generation.

The default template without explicit inputs keeps its existing behavior. A
service using shared option descriptors should select its owned declarations
with `paths`, so it imports those descriptors without generating duplicate Go
registrations. Its sibling client contracts belong in another declared input,
so replacing the generated output also recreates their bindings.

When refreshing a marked single-service scaffold, the agent reads `go_package`
from the proto that declares that service. Listener imports follow its import
path and package name, including versioned packages; they do not revert to the
factory's default `service/pkg/gen`. With no explicit option the factory default
is retained. Buf-managed rewrites or plugin import mappings must agree with the
service's declared `go_package` for those generated listeners.
