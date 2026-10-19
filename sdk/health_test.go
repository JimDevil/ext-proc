package extproc

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	hpb "google.golang.org/grpc/health/grpc_health_v1"
)

func TestHealthServerStateTransitions(t *testing.T) {
	t.Parallel()

	hs := NewHealthServer()
	res, err := hs.Check(context.Background(), &hpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.GetStatus() != hpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("new health server must start NOT_SERVING, got %v", res.GetStatus())
	}
	hs.MarkReady()
	if hs.GetStatus() != hpb.HealthCheckResponse_SERVING {
		t.Fatal("MarkReady did not update the status")
	}
	hs.MarkUnready()
	if hs.GetStatus() != hpb.HealthCheckResponse_NOT_SERVING {
		t.Fatal("MarkUnready did not update the status")
	}
}

// fakeWatchStream implements hpb.Health_WatchServer for the Watch test.
type fakeWatchStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent chan hpb.HealthCheckResponse_ServingStatus
}

func (s *fakeWatchStream) Send(resp *hpb.HealthCheckResponse) error {
	s.sent <- resp.GetStatus()
	return nil
}

func (s *fakeWatchStream) Context() context.Context { return s.ctx }

func (s *fakeWatchStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeWatchStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeWatchStream) SetTrailer(metadata.MD)       {}
func (s *fakeWatchStream) SendMsg(any) error            { return nil }
func (s *fakeWatchStream) RecvMsg(any) error            { return nil }

func TestHealthWatchStreamsTransitions(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &fakeWatchStream{ctx: ctx, sent: make(chan hpb.HealthCheckResponse_ServingStatus, 4)}
	hs := NewReadyHealthServer()

	errCh := make(chan error, 1)
	go func() { errCh <- hs.Watch(&hpb.HealthCheckRequest{}, stream) }()

	if got := receiveStatus(t, stream.sent); got != hpb.HealthCheckResponse_SERVING {
		t.Fatalf("initial watch status mismatch: %v", got)
	}
	hs.MarkUnready()
	if got := receiveStatus(t, stream.sent); got != hpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("drain watch status mismatch: %v", got)
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Watch returned an error after cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after context cancellation")
	}
}

func receiveStatus(t *testing.T, ch <-chan hpb.HealthCheckResponse_ServingStatus) hpb.HealthCheckResponse_ServingStatus {
	t.Helper()
	select {
	case status := <-ch:
		return status
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a health status")
		return hpb.HealthCheckResponse_UNKNOWN
	}
}
