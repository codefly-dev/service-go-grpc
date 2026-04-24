package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
)

// registerCommands registers agent-specific commands.
// NOTE: test and lint are standard Runtime RPCs — don't duplicate here.
// Only register commands that are unique to this agent type.
func (s *Runtime) registerCommands() {
	s.RegisterCommand(&agentv0.CommandDefinition{
		Name:        "proto",
		Description: "Regenerate protobuf Go code from .proto files via buf",
		Tags:        []string{"codegen", "proto"},
		Aliases:     []string{"generate", "buf"},
	}, s.cmdProto)

	s.RegisterCommand(&agentv0.CommandDefinition{
		Name:        "health",
		Description: "Check if the gRPC service is responding",
		Tags:        []string{"health", "diagnostic"},
	}, s.cmdHealth)

	// grpcurl family — introspect and invoke the running service's
	// gRPC endpoint. All three use reflection over plaintext; if the
	// service has TLS or reflection disabled, users can pass custom
	// flags via `grpcurl` and override. Mode-consistent: runs in the
	// plugin's active backend so the grpcurl binary's ability to reach
	// the service matches the execution environment.
	s.RegisterCommand(&agentv0.CommandDefinition{
		Name:        "grpcurl-list",
		Description: "List gRPC services exposed by the running server (requires reflection).",
		Tags:        []string{"grpc", "introspect"},
		Aliases:     []string{"grpc-list", "list-services"},
	}, s.cmdGrpcurlList)

	s.RegisterCommand(&agentv0.CommandDefinition{
		Name:        "grpcurl-describe",
		Description: "Describe a gRPC service or method. Pass the fully-qualified name as the first arg (e.g. `package.Service` or `package.Service.Method`). Without args, describes all services.",
		Usage:       `grpcurl-describe my.package.MyService`,
		Tags:        []string{"grpc", "introspect"},
		Aliases:     []string{"grpc-describe"},
	}, s.cmdGrpcurlDescribe)

	s.RegisterCommand(&agentv0.CommandDefinition{
		Name:        "grpcurl",
		Description: "Invoke a gRPC method on the running server. Args: <method> [json-payload]. Plaintext + reflection; for custom flags edit the command or pipe directly.",
		Usage:       `grpcurl my.package.MyService.MyMethod '{"field":"value"}'`,
		Tags:        []string{"grpc", "invoke"},
		Aliases:     []string{"grpc-invoke", "grpc-call"},
	}, s.cmdGrpcurlInvoke)
}

func (s *Runtime) cmdProto(ctx context.Context, _ []string) (string, error) {
	// Route `buf generate` through the plugin's active RunnerEnvironment
	// (s.GoGrpc.Service.ActiveEnv) so proto compilation runs in the same
	// mode (native/docker/nix) as the rest of the service — including
	// the buf + protoc + language plugin subprocesses. Fallback to
	// native when ActiveEnv is nil (pre-Init) so the command still works
	// standalone.
	protoDir := filepath.Clean(filepath.Join(s.Service.SourceLocation, "..", "proto"))
	env := s.GoGrpc.Service.ActiveEnv
	if env == nil {
		native, nerr := runners.NewNativeEnvironment(ctx, protoDir)
		if nerr != nil {
			return "", fmt.Errorf("cannot create runner environment: %w", nerr)
		}
		env = native
	}
	proc, err := env.NewProcess("buf", "generate")
	if err != nil {
		return "", fmt.Errorf("cannot create buf process: %w", err)
	}
	proc.WithDir(protoDir)
	var buf bytes.Buffer
	proc.WithOutput(&buf)
	if runErr := proc.Run(ctx); runErr != nil {
		return buf.String(), fmt.Errorf("proto generation failed: %w", runErr)
	}
	return "Proto code regenerated successfully\n" + buf.String(), nil
}

func (s *Runtime) cmdHealth(_ context.Context, _ []string) (string, error) {
	if s.runner == nil {
		return "NOT RUNNING", nil
	}
	return "RUNNING", nil
}

// cmdGrpcurlList: `grpcurl -plaintext <addr> list`.
func (s *Runtime) cmdGrpcurlList(ctx context.Context, _ []string) (string, error) {
	return s.runGrpcurl(ctx, "list")
}

// cmdGrpcurlDescribe: `grpcurl -plaintext <addr> describe [target]`.
// Without a target, describes all reflected services.
func (s *Runtime) cmdGrpcurlDescribe(ctx context.Context, args []string) (string, error) {
	extra := []string{"describe"}
	if len(args) > 0 {
		extra = append(extra, args[0])
	}
	return s.runGrpcurl(ctx, extra...)
}

// cmdGrpcurlInvoke: `grpcurl -plaintext -d <json> <addr> <method>`.
// Accepts `<method>` or `<method> <json>`.
func (s *Runtime) cmdGrpcurlInvoke(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("grpcurl invoke requires a method as the first argument (e.g. package.Service.Method)")
	}
	method := args[0]
	extra := []string{}
	if len(args) >= 2 {
		extra = append(extra, "-d", args[1])
	}
	extra = append(extra, method)
	return s.runGrpcurl(ctx, extra...)
}

// runGrpcurl builds `grpcurl -plaintext <host>:<port> <extraArgs...>` and
// executes via the plugin's ActiveEnv — same mode (native/docker/nix)
// as the service itself. Routes through NativeProc for pgid tracking
// so an accidental Ctrl-C on the CLI doesn't orphan a stuck RPC.
func (s *Runtime) runGrpcurl(ctx context.Context, extra ...string) (string, error) {
	addr, err := s.grpcAddress(ctx)
	if err != nil {
		return "", err
	}
	env := s.GoGrpc.Service.ActiveEnv
	if env == nil {
		native, nerr := runners.NewNativeEnvironment(ctx, s.Service.SourceLocation)
		if nerr != nil {
			return "", fmt.Errorf("cannot create runner environment: %w", nerr)
		}
		env = native
	}
	args := append([]string{"-plaintext", addr}, extra...)
	proc, err := env.NewProcess("grpcurl", args...)
	if err != nil {
		return "", fmt.Errorf("cannot create grpcurl process (is grpcurl installed?): %w", err)
	}
	var buf bytes.Buffer
	proc.WithOutput(&buf)
	if runErr := proc.Run(ctx); runErr != nil {
		return buf.String(), fmt.Errorf("grpcurl failed: %w", runErr)
	}
	return buf.String(), nil
}

// grpcAddress resolves the running service's gRPC endpoint to a host:port
// string for grpcurl. Returns an error if the service hasn't started yet
// (no network mapping available) so users see a clear "start the service
// first" hint instead of an obscure grpcurl error.
func (s *Runtime) grpcAddress(ctx context.Context) (string, error) {
	if s.runner == nil {
		return "", fmt.Errorf("service is not running — start it first")
	}
	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.GoGrpc.GrpcEndpoint, resources.NewNativeNetworkAccess())
	if err != nil {
		return "", fmt.Errorf("cannot resolve grpc endpoint: %w", err)
	}
	return instance.Address, nil
}
