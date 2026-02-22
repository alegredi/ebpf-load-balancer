package healthcheck

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"proxy/internal/balancer"
	"time"
)

var healthClient = &http.Client{
	Timeout: 2 * time.Second,
	Transport: &http.Transport{
		DisableKeepAlives: true,
	},
}

func checkBackend(b *balancer.Backend) bool {
	if b.HealthEndpoint != "" {
		resp, err := healthClient.Get(b.URL + b.HealthEndpoint)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}

	u, err := url.Parse(b.URL)
	if err != nil {
		return false
	}
	conn, err := net.DialTimeout("tcp", u.Host, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// StartHealthCheck starts health checks for all backends with graceful context cancellation.
func StartHealthCheck(ctx context.Context, lb *balancer.LoadBalancer, interval time.Duration) {
	for _, backend := range lb.Backends {
		go func(b *balancer.Backend) {
			alive := checkBackend(b)
			b.SetAlive(alive)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					alive := checkBackend(b)
					b.SetAlive(alive)
				}
			}
		}(backend)
	}
}
