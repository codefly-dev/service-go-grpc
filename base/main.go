package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/go-grpc/base/pkg/adapters"
	codefly "github.com/codefly-dev/sdk-go"
)

func Must[T any](t T, err error) T {
	if err != nil {
		panic(err)
	}
	return t
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	provider, err := codefly.Init(ctx)
	ctx = provider.Inject(ctx)

	defer codefly.CatchPanic(ctx)

	w := wool.Get(ctx).In("main")

	// For running outside of codefly
	// Load overrides
	err = codefly.LoadOverrides(ctx)
	if err != nil {
		w.Error("cannot load overrides", wool.Field("error", err))
	}

	config := &adapters.Configuration{
		EndpointGrpc: Must(Must(codefly.GetEndpoint(ctx, "self/grpc")).PortAddress()),
	}
	if endpoint, err := codefly.GetEndpoint(ctx, "self/rest"); err == nil {
		config.EndpointHttp = Must(endpoint.PortAddress())
	}

	server, err := adapters.NewServer(config)
	if err != nil {
		panic(err)
	}

	go func() {
		err = server.Start(context.Background())
		if err != nil {
			panic(err)
		}
	}()

	<-ctx.Done()
	server.Stop()
	fmt.Println("got interruption signal")

}
