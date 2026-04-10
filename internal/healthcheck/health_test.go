package healthcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"proxy/internal/balancer"
	"sync/atomic"
	"testing"
	"time"
)

func TestBackendHysteresis(t *testing.T) {
	b := &balancer.Backend{URL: "http://test"}
	b.SetAlive(true)

	// Single failure shouldn't trigger transition (threshold is 2)
	trans := b.MarkFailure(2)
	if trans || !b.IsAlive() {
		t.Errorf("backend transitioned to dead on first failure")
	}

	// Second consecutive failure should trigger transition
	trans = b.MarkFailure(2)
	if !trans || b.IsAlive() {
		t.Errorf("backend did not transition to dead on second failure")
	}

	// Single success shouldn't bring it up immediately
	trans = b.MarkSuccess(2)
	if trans || b.IsAlive() {
		t.Errorf("backend became alive on first success")
	}

	// Second success brings it up
	trans = b.MarkSuccess(2)
	if !trans || !b.IsAlive() {
		t.Errorf("backend failed to transition to alive on second success")
	}
}

func TestHealthCheckLoop(t *testing.T) {
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	b := &balancer.Backend{
		URL:            server.URL,
		HealthEndpoint: "/",
	}
	lb := &balancer.LoadBalancer{
		Backends: []*balancer.Backend{b},
	}

	ctx, cancel := context.WithCancel(context.Background())
	var statusCalls int32

	StartHealthCheck(ctx, lb, 20*time.Millisecond, func(backend *balancer.Backend, alive bool) {
		atomic.AddInt32(&statusCalls, 1)
	})

	time.Sleep(70 * time.Millisecond)
	cancel()

	if atomic.LoadInt32(&requestCount) == 0 {
		t.Errorf("expected health check requests to be made")
	}
	if atomic.LoadInt32(&statusCalls) == 0 {
		t.Errorf("expected status callback to be triggered")
	}
}
