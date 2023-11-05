package adapters

import (
	"context"
	"fmt"
	"net"

	"github.com/bufbuild/protovalidate-go"
	gen "github.com/codefly-dev/go-grpc/base/adapters/v1"
	codefly "github.com/codefly-dev/sdk-go"
	"google.golang.org/grpc"
)

type Configuration struct {
	EndpointGrpc string
	EndpointHttp string
}

type GrpcServer struct {
	gen.UnimplementedWebServer
	configuration *Configuration
	gRPC          *grpc.Server
	validator     *protovalidate.Validator
}

func (s *GrpcServer) Version(ctx context.Context, req *gen.VersionRequest) (*gen.VersionResponse, error) {
	return &gen.VersionResponse{
		Version: codefly.Version(),
	}, nil
}

func NewGrpServer(c *Configuration) (*GrpcServer, error) {
	grpcServer := grpc.NewServer()
	v, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("failed to create validator: %w", err)
	}

	s := GrpcServer{
		configuration: c,
		gRPC:          grpcServer,
		validator:     v,
	}
	gen.RegisterWebServer(grpcServer, &s)
	return &s, nil
}

func (s *GrpcServer) Run(ctx context.Context) error {
	fmt.Println("Starting gRPC server at", s.configuration.EndpointGrpc)
	lis, err := net.Listen("tcp", s.configuration.EndpointGrpc)
	if err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}

	if err := s.gRPC.Serve(lis); err != nil {
		return fmt.Errorf("failed to serve: %s", err)
	}
	return nil
}
