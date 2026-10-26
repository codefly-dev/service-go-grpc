package main

import (
	"context"
	"fmt"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
)

// discoverEndpoints uses Core's conventional endpoint selection, including its
// ambiguity checks. A Connect listener already serves native gRPC, so it does
// not require a second endpoint or a second listener.
func (s *Service) discoverEndpoints(ctx context.Context) error {
	s.GrpcEndpoint, s.RestEndpoint, s.ConnectEndpoint = nil, nil, nil
	var err error
	if len(resources.FindEndpointsByAPI(ctx, standards.GRPC, s.Endpoints)) > 0 {
		s.GrpcEndpoint, err = resources.FindGRPCEndpoint(ctx, s.Endpoints)
		if err != nil {
			return err
		}
	}
	if s.Settings.ConnectEndpoint {
		s.ConnectEndpoint, err = resources.FindConnectEndpoint(ctx, s.Endpoints)
		if err != nil {
			return err
		}
	}
	if s.rpcEndpoint() == nil {
		return fmt.Errorf("no RPC listener: declare a grpc endpoint or a connect endpoint with connect-endpoint enabled")
	}
	if s.Settings.RestEndpoint {
		s.RestEndpoint, err = resources.FindRestEndpoint(ctx, s.Endpoints)
		if err != nil {
			return err
		}
	}
	return nil
}

// rpcEndpoint is the declared listener grpcurl and health calls can reach.
func (s *Service) rpcEndpoint() *basev0.Endpoint {
	if s.GrpcEndpoint != nil {
		return s.GrpcEndpoint
	}
	return s.ConnectEndpoint
}
