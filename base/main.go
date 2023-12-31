package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/codefly-dev/go-grpc/base/adapters"
	codefly "github.com/codefly-dev/sdk-go"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	provider, err := codefly.Init(ctx)
	ctx = provider.Inject(ctx)

	defer codefly.CatchPanic(ctx)

	config := &adapters.Configuration{
		EndpointGrpc: codefly.Endpoint(ctx, "self/grpc").PortAddress(),
	}
	if codefly.Endpoint(ctx, "self/rest").IsPresent() {
		config.EndpointHttp = codefly.Endpoint(ctx, "self/rest").PortAddress()
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
