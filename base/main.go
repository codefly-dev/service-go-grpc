package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/codefly-dev/go-grpc/base/adapters"
	codefly "github.com/codefly-dev/sdk-go"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	codefly.WithTrace()
	defer codefly.CatchPanic()

	config := &adapters.Configuration{
		EndpointGrpc: codefly.Endpoint("self::grpc").PortAddress(),
	}
	if codefly.Endpoint("self::http").IsPresent() {
		config.EndpointHttp = codefly.Endpoint("self::http").PortAddress()
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

func multiSignalHandler(signal os.Signal) {
	switch signal {
	case syscall.SIGHUP:
	case syscall.SIGINT:
		log.Println("Signal:", signal.String())
		log.Println("Interrupt by Ctrl+C")
		os.Exit(0)
	case syscall.SIGTERM:
		log.Println("Signal:", signal.String())
		log.Println("Process is killed.")
		os.Exit(0)
	default:
		log.Println("Unhandled/unknown signal")
	}
}
