// Package connecthandler implements the Connect-RPC service interfaces
// generated from the vectorless proto definitions.
//
// These handlers run alongside the hand-written chi REST handlers,
// giving clients a choice of three transports from one server: plain
// HTTP/JSON (Connect protocol), gRPC, and gRPC-Web.
package connecthandler

import (
	"context"

	"connectrpc.com/connect"

	v1 "github.com/hallelx2/vectorless-engine/gen/vectorless/v1"
	"github.com/hallelx2/vectorless-engine/gen/vectorless/v1/vectorlessv1connect"
)

// HealthService implements vectorlessv1connect.HealthServiceHandler.
type HealthService struct {
	vectorlessv1connect.UnimplementedHealthServiceHandler
	version string
}

// NewHealthService creates a HealthService.
func NewHealthService(version string) *HealthService {
	return &HealthService{version: version}
}

// Check returns {"status": "ok"}.
func (s *HealthService) Check(
	_ context.Context,
	_ *connect.Request[v1.HealthCheckRequest],
) (*connect.Response[v1.HealthCheckResponse], error) {
	return connect.NewResponse(&v1.HealthCheckResponse{
		Status: "ok",
	}), nil
}

// Version returns the build version.
func (s *HealthService) Version(
	_ context.Context,
	_ *connect.Request[v1.VersionRequest],
) (*connect.Response[v1.VersionResponse], error) {
	return connect.NewResponse(&v1.VersionResponse{
		Version: s.version,
	}), nil
}
