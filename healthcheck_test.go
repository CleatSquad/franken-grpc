package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc/health"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
)

// waitForStatus polls the health server until it reports want, or fails the
// test once timeout elapses — the poller runs on its own ticker, so the
// test cannot assume a fixed number of ticks have already happened.
func waitForStatus(t *testing.T, healthServer *health.Server, want grpc_health_v1.HealthCheckResponse_ServingStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := healthServer.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{})
		if err == nil && resp.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("health status did not reach %v within %v", want, timeout)
}

// runPollerForTest starts pollBackendHealth in the background and returns a
// stop func that cancels it and blocks until the goroutine has actually
// returned — restoring the package-level backend/interval/timeout vars
// before that point would race with pollBackendHealth's own reads of them.
func runPollerForTest(healthServer *health.Server, client *http.Client) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pollBackendHealth(ctx, healthServer, client)
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
}

func TestPollBackendHealth_MarksNotServingWhenBackendUnreachable(t *testing.T) {
	origBackend := basePHPBackendURL
	origInterval := healthInterval
	origTimeout := healthTimeout

	// No listener on this address: connections are refused immediately.
	basePHPBackendURL = "http://127.0.0.1:1"
	healthInterval = 10 * time.Millisecond
	healthTimeout = 50 * time.Millisecond

	healthServer := health.NewServer()
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	stop := runPollerForTest(healthServer, &http.Client{})
	waitForStatus(t, healthServer, grpc_health_v1.HealthCheckResponse_NOT_SERVING, time.Second)
	stop()

	basePHPBackendURL = origBackend
	healthInterval = origInterval
	healthTimeout = origTimeout
}

func TestPollBackendHealth_RecoversToServing(t *testing.T) {
	origBackend := basePHPBackendURL
	origInterval := healthInterval
	origTimeout := healthTimeout

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	basePHPBackendURL = backend.URL
	healthInterval = 10 * time.Millisecond
	healthTimeout = 50 * time.Millisecond

	healthServer := health.NewServer()
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)

	stop := runPollerForTest(healthServer, &http.Client{})
	waitForStatus(t, healthServer, grpc_health_v1.HealthCheckResponse_SERVING, time.Second)
	stop()

	basePHPBackendURL = origBackend
	healthInterval = origInterval
	healthTimeout = origTimeout
}

func TestPollBackendHealth_StopsOnContextCancel(t *testing.T) {
	origBackend := basePHPBackendURL
	origInterval := healthInterval
	t.Cleanup(func() {
		basePHPBackendURL = origBackend
		healthInterval = origInterval
	})

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	basePHPBackendURL = backend.URL
	healthInterval = 5 * time.Millisecond

	healthServer := health.NewServer()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pollBackendHealth(ctx, healthServer, &http.Client{})
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pollBackendHealth did not return after context cancellation")
	}
}
