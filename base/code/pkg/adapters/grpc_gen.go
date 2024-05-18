package adapters

/* -----------------------------------------------------------------

⚠️ This code is generated by the agent. Do not edit this file!

Recommendation: create a `rpcs.go` file in the same directory as this file and
implement your APIs there.

----------------------------------------------------------------- */

import (
	"context"
	"fmt"
	"github.com/bufbuild/protovalidate-go"
	"github.com/codefly-dev/go-grpc/base/pkg/gen"
	"net"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"google.golang.org/protobuf/proto"

	"google.golang.org/grpc/reflection"

	codefly "github.com/codefly-dev/sdk-go"
	"google.golang.org/grpc"
)

var validator *protovalidate.Validator

func init() {
	v, err := protovalidate.New()
	if err != nil {
		panic(fmt.Errorf("failed to create validator: %w", err))
	}
	validator = v
}

func Validate(req proto.Message) error {
	err := validator.Validate(req)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}

func (s *GrpcServer) Version(ctx context.Context, req *gen.VersionRequest) (*gen.VersionResponse, error) {
	if err := Validate(req); err != nil {
		return nil, err
	}
	return &gen.VersionResponse{
		Version: codefly.Version(),
	}, nil
}

type Configuration struct {
	EndpointGrpcPort uint16
	EndpointHttpPort *uint16
}

type GrpcServer struct {
	gen.UnsafeWebServiceServer
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
	fmt.Println("Starting gRPC server at", s.configuration.EndpointGrpcPort)
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", s.configuration.EndpointGrpcPort))
	if err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}

	if err := s.gRPC.Serve(lis); err != nil {
		return fmt.Errorf("failed to serve: %s", err)
	}
	return nil
}