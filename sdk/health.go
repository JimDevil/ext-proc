package extproc

import (
	"context"
	"sync"
	"sync/atomic"

	hpb "google.golang.org/grpc/health/grpc_health_v1"
)

// HealthServer is the standard gRPC health-check service with runtime state
// switching: graceful shutdown marks it unready first (drain), then stops.
type HealthServer struct {
	hpb.UnimplementedHealthServer
	status atomic.Int32

	mu       sync.Mutex
	watchers map[chan hpb.HealthCheckResponse_ServingStatus]struct{}
}

func NewHealthServer() *HealthServer {
	return newHealthServer(hpb.HealthCheckResponse_NOT_SERVING)
}

// NewReadyHealthServer starts as SERVING (the normal case).
func NewReadyHealthServer() *HealthServer {
	return newHealthServer(hpb.HealthCheckResponse_SERVING)
}

func newHealthServer(s hpb.HealthCheckResponse_ServingStatus) *HealthServer {
	hs := &HealthServer{watchers: make(map[chan hpb.HealthCheckResponse_ServingStatus]struct{})}
	hs.status.Store(int32(s))
	return hs
}

func (s *HealthServer) set(v hpb.HealthCheckResponse_ServingStatus) {
	s.status.Store(int32(v))
	s.mu.Lock()
	for ch := range s.watchers {
		select {
		case ch <- v:
		default:
			// The watcher is slow; it will observe the latest value on its
			// next receive. Never block the health state transition.
		}
	}
	s.mu.Unlock()
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

// Watch implements grpc_health_v1.HealthServer: it sends the current status
// immediately and then every transition (including the drain transition on
// graceful shutdown, which lets long-lived clients observe NOT_SERVING).
func (s *HealthServer) Watch(_ *hpb.HealthCheckRequest, srv hpb.Health_WatchServer) error {
	ch := make(chan hpb.HealthCheckResponse_ServingStatus, 1)
	s.mu.Lock()
	if s.watchers == nil {
		s.watchers = make(map[chan hpb.HealthCheckResponse_ServingStatus]struct{})
	}
	s.watchers[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.watchers, ch)
		s.mu.Unlock()
	}()

	if err := srv.Send(&hpb.HealthCheckResponse{Status: s.GetStatus()}); err != nil {
		return err
	}
	for {
		select {
		case <-srv.Context().Done():
			return nil
		case status := <-ch:
			if err := srv.Send(&hpb.HealthCheckResponse{Status: status}); err != nil {
				return err
			}
		}
	}
}
