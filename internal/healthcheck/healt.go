package healthcheck

import (
	"net"
	"net/http"
	"net/url"
	"proxy/internal/balancer"
	"time"
)

func checkBackend(b *balancer.Backend) bool {
	if b.HealthEndpoint != "" {
		client := http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(b.URL + b.HealthEndpoint)
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

func StartHealthCheck(lb *balancer.LoadBalancer, interval time.Duration) {
	for _, backend := range lb.Backends {
		go func(b *balancer.Backend) {
			alive := checkBackend(b)
			b.SetAlive(alive)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()

			for range ticker.C {
				alive := checkBackend(b)
				b.SetAlive(alive)
			}
		}(backend)
	}
}
