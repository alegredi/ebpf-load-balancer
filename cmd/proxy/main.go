package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"proxy/internal/balancer"
	"proxy/internal/config"
	"proxy/internal/healthcheck"
	"proxy/pkg/cache"
	"sync"
	"syscall"
	"time"
)

var bufPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	body       *bytes.Buffer
}

func (r *responseRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.body != nil {
		r.body.Write(b)
	}
	return r.ResponseWriter.Write(b)
}

func main() {
	log.SetOutput(os.Stdout)
	log.Println("[INFO] Starting Reverse Proxy...")

	cfg, err := config.LoadConfig("config.yaml")
	if err != nil {
		log.Fatalf("cannot load config: %v", err)
	}

	cacheInvalidationTimer, err := time.ParseDuration(cfg.CacheInvalidationTimer)
	if err != nil {
		cacheInvalidationTimer = 30 * time.Second
	}

	healthcheckInterval := cfg.HealthCheckDuration()
	cacheLRU := cache.NewLRUCache(128)

	var backends []*balancer.Backend
	for _, bcfg := range cfg.Backends {
		targetURL, err := url.Parse(bcfg.URL)
		if err != nil {
			log.Fatalf("invalid backend url %s: %v", bcfg.URL, err)
		}

		proxy := httputil.NewSingleHostReverseProxy(targetURL)
		proxy.Transport = &http.Transport{
			MaxIdleConns:          256,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		}

		origDirector := proxy.Director
		proxy.Director = func(req *http.Request) {
			origDirector(req)
			req.Host = targetURL.Host
		}

		proxy.ErrorHandler = func(rw http.ResponseWriter, r *http.Request, e error) {
			log.Printf("[ERROR] Backend error for %s: %v", bcfg.URL, e)
			http.Error(rw, "bad gateway", http.StatusBadGateway)
		}

		backends = append(backends, &balancer.Backend{
			URL:            bcfg.URL,
			HealthEndpoint: bcfg.HealthEndpoint,
			Proxy:          proxy,
		})
	}

	lb := &balancer.LoadBalancer{
		Backends: backends,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	healthcheck.StartHealthCheck(ctx, lb, healthcheckInterval, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			key := cache.CacheKey(r)
			if cached, ok := cacheLRU.Get(key); ok {
				for k, vv := range cached.Header {
					for _, v := range vv {
						w.Header().Add(k, v)
					}
				}
				w.Header().Set("X-Cache", "HIT")
				w.WriteHeader(cached.StatusCode)
				_, _ = w.Write(cached.Body)
				return
			}

			backend, err := lb.GetLeastConnBackend()
			if err != nil {
				http.Error(w, "no available backends", http.StatusServiceUnavailable)
				return
			}
			backend.IncConn()
			defer backend.DecConn()

			buf := bufPool.Get().(*bytes.Buffer)
			buf.Reset()
			defer bufPool.Put(buf)

			rec := &responseRecorder{
				ResponseWriter: w,
				statusCode:     http.StatusOK,
				body:           buf,
			}
			w.Header().Set("X-Cache", "MISS")
			backend.Proxy.ServeHTTP(rec, r)

			if rec.statusCode >= 200 && rec.statusCode < 300 {
				_ = cacheLRU.Put(key, &cache.CachedResponse{
					StatusCode: rec.statusCode,
					Header:     w.Header().Clone(),
					Body:       buf.Bytes(),
					ExpiresAt:  time.Now().Add(cacheInvalidationTimer),
				})
			}
			return
		}

		backend, err := lb.GetLeastConnBackend()
		if err != nil {
			http.Error(w, "no available backends", http.StatusServiceUnavailable)
			return
		}
		backend.IncConn()
		defer backend.DecConn()
		backend.Proxy.ServeHTTP(w, r)
	})

	// Periodic status log
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, b := range lb.Backends {
					log.Printf("[STATUS] Backend: %s | ActiveConns: %d | Alive: %t",
						b.URL, b.GetActiveConnections(), b.IsAlive())
				}
			}
		}
	}()

	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("[INFO] Proxy listening on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	log.Println("[INFO] Shutting down proxy server gracefully...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("[ERROR] Server forced to shutdown: %v", err)
	}
	log.Println("[INFO] Server exited successfully.")
}

// Suppress unused import warnings if any
var _ = io.Discard
