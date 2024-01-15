package adapters

import (
	"context"
	"fmt"
	"net/http"

	"github.com/rs/cors"

	gen "github.com/codefly-dev/go-grpc/base/proto/go"
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

	// Create a CORS handler
	c := cors.New(cors.Options{
		AllowedOrigins: []string{"*"}, // Allow all origins
		AllowedMethods: []string{"GET", "POST", "PUT", "DELETE", "PATCH"},
		AllowedHeaders: []string{"*"}, // Allow all headers
	})

	gwMux := runtime.NewServeMux()

	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}

	err := gen.RegisterWebServiceHandlerFromEndpoint(ctx, gwMux, s.config.EndpointGrpc, opts)
	if err != nil {
		return err
	}

	// Wrap your mux with the CORS handler
	handler := c.Handler(gwMux)

	return http.ListenAndServe(s.config.EndpointHttp, handler)
}
