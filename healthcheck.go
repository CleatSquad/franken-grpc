package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"google.golang.org/grpc/health"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
)

var (
	healthPath     = getEnv("PHP_BACKEND_HEALTH_PATH", "/")
	healthInterval = getEnvDuration("PHP_BACKEND_HEALTH_INTERVAL", 5*time.Second)
	healthTimeout  = getEnvDuration("PHP_BACKEND_HEALTH_TIMEOUT", 2*time.Second)
)

func getEnvDuration(key string, defaultVal time.Duration) time.Duration {
	val, err := time.ParseDuration(getEnv(key, ""))
	if err != nil {
		return defaultVal
	}
	return val
}

// pollBackendHealth periodically checks basePHPBackendURL and reflects the
// result on the gRPC health service, so grpc_health_v1's overall ("") status
// tells the truth about the PHP backend instead of being permanently
// SERVING regardless of whether relayToPHP can actually reach it — the gap
// php-fpm's pool manager closes by knowing which workers are alive, which
// this relay had no equivalent of.
func pollBackendHealth(ctx context.Context, healthServer *health.Server, client *http.Client) {
	url := basePHPBackendURL + healthPath
	check := func() {
		reqCtx, cancel := context.WithTimeout(ctx, healthTimeout)
		defer cancel()

		req, err := http.NewRequestWithContext(reqCtx, http.MethodHead, url, nil)
		if err != nil {
			log.Printf("health check: failed to build request: %v", err)
			return
		}

		resp, err := client.Do(req)
		if err != nil {
			healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
			return
		}
		_ = resp.Body.Close()

		// Any response at all — including a 404 on a path the backend
		// doesn't route — proves the PHP process is up and answering
		// HTTP, which is what this check exists to know. Only a
		// transport-level failure (connection refused, timeout) means
		// the backend is actually down.
		healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	}

	check()

	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}
