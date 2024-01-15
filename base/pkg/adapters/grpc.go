package adapters

import (
	"context"
	"fmt"
	"net"

	"google.golang.org/grpc/reflection"

	"github.com/bufbuild/protovalidate-go"
	gen "github.com/codefly-dev/go-grpc/base/proto/go"
	codefly "github.com/codefly-dev/sdk-go"
	"google.golang.org/grpc"
)

func (s *GrpcServer) Version(ctx context.Context, req *gen.VersionRequest) (*gen.VersionResponse, error) {
	return &gen.VersionResponse{
		Version: codefly.Version(),
	}, nil
}

type Configuration struct {
	EndpointGrpc string
	EndpointHttp string
}

type GrpcServer struct {
	gen.UnimplementedWebServiceServer
	configuration *Configuration
	gRPC          *grpc.Server
	validator     *protovalidate.Validator
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
	gen.RegisterWebServiceServer(grpcServer, &s)
	reflection.Register(grpcServer)
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
