package extproc

import (
	"context"
	"sync/atomic"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hpb "google.golang.org/grpc/health/grpc_health_v1"
)

// HealthServer is the standard gRPC health-check service with runtime state
// switching: graceful shutdown marks it unready first (drain), then stops.
type HealthServer struct {
	hpb.UnimplementedHealthServer
	status atomic.Int32
}

func NewHealthServer() *HealthServer {
	return newHealthServer(hpb.HealthCheckResponse_NOT_SERVING)
}

// NewReadyHealthServer starts as SERVING (the normal case).
func NewReadyHealthServer() *HealthServer {
	return newHealthServer(hpb.HealthCheckResponse_SERVING)
}

func newHealthServer(s hpb.HealthCheckResponse_ServingStatus) *HealthServer {
	hs := &HealthServer{}
	hs.status.Store(int32(s))
	return hs
}

func (s *HealthServer) set(v hpb.HealthCheckResponse_ServingStatus) {
	s.status.Store(int32(v))
}

func (s *HealthServer) GetStatus() hpb.HealthCheckResponse_ServingStatus {
	return hpb.HealthCheckResponse_ServingStatus(s.status.Load())
}

// MarkReady flags ready (accept new streams again).
func (s *HealthServer) MarkReady() {
	s.set(hpb.HealthCheckResponse_SERVING)
}

// MarkUnready flags unready (no new streams; existing streams continue).
func (s *HealthServer) MarkUnready() {
	s.set(hpb.HealthCheckResponse_NOT_SERVING)
}

// Check implements grpc_health_v1.HealthServer.
func (s *HealthServer) Check(ctx context.Context, req *hpb.HealthCheckRequest) (*hpb.HealthCheckResponse, error) {
	return &hpb.HealthCheckResponse{Status: s.GetStatus()}, nil
}

// Watch implements grpc_health_v1.HealthServer (streaming watch unimplemented).
func (s *HealthServer) Watch(req *hpb.HealthCheckRequest, srv hpb.Health_WatchServer) error {
	return status.Error(codes.Unimplemented, "Watch is not implemented")
}
