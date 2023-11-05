package adapters

import (
	"context"
	"fmt"
	"net/http"

	gen "github.com/codefly-dev/go-grpc/base/adapters/v1"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type RestServer struct {
	config *Configuration
}

func NewRestServer(c *Configuration) (*RestServer, error) {
	server := &RestServer{config: c}
	// Start Rest server (and proxy calls to gRPC server endpoint)
	return server, nil
}

func (s *RestServer) Run(ctx context.Context) error {
	fmt.Println("Starting Rest server at", s.config.EndpointHttp)
	gwMux := runtime.NewServeMux()

	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	err := gen.RegisterWebHandlerFromEndpoint(ctx, gwMux, s.config.EndpointGrpc, opts)
	if err != nil {
		return err
	}
	return http.ListenAndServe(s.config.EndpointHttp, gwMux)
}
