package adapters

/* -----------------------------------------------------------------

⚠️ This code is generated by the agent. Do not edit this file!

----------------------------------------------------------------- */

import (
	"context"
	"fmt"
	"github.com/codefly-dev/core/standards/headers"
	"github.com/codefly-dev/go-grpc/base/pkg/gen"
	"net/http"

	"github.com/codefly-dev/core/wool"

	"google.golang.org/grpc/metadata"

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

func CustomHeaderToGRPCMetadataAnnotator(ctx context.Context, req *http.Request) metadata.MD {
	var data []string
	for _, h := range []string{headers.UserID, headers.UserEmail} {
		if v := req.Header.Get(h); len(v) > 0 {
			data = append(data, wool.HeaderKey(h), v)
		}
	}
	return metadata.Pairs(data...)
}

func (s *RestServer) Run(ctx context.Context) error {
	fmt.Println("Starting Rest server at", *s.config.EndpointHttpPort)

	// Create a CORS handler
	c := Cors()

	gwMux := runtime.NewServeMux(runtime.WithMetadata(CustomHeaderToGRPCMetadataAnnotator))

	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}

	err := gen.RegisterSvcServiceHandlerFromEndpoint(ctx, gwMux, fmt.Sprintf("0.0.0.0:%d", s.config.EndpointGrpcPort), opts)
	if err != nil {
		return err
	}

	// Wrap your mux with the CORS handler
	handler := c.Handler(gwMux)

	return http.ListenAndServe(fmt.Sprintf(":%d", *s.config.EndpointHttpPort), handler)
}
