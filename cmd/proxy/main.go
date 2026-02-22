package main

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"proxy/internal/balancer"
	"proxy/internal/config"
	"proxy/internal/healthcheck"
	"proxy/pkg/cache"
	"time"
)

func main() {
	log.SetOutput(os.Stdout)

	cacheLRU := cache.NewLRUCache(2)

	// reads config
	cfg, err := config.LoadConfig("config.yaml")
	if err != nil {
		log.Fatalf("cannot load config: %v", err)
	}

	cacheInvalidationTimer, err := time.ParseDuration(cfg.CacheInvalidationTimer)
	if err != nil {
		cacheInvalidationTimer, _ = time.ParseDuration("30s")
	}

	healthcheckInterval, err := time.ParseDuration(cfg.HealthCheckInterval)
	if err != nil {
		healthcheckInterval, _ = time.ParseDuration("30s")
	}

	var backends []*balancer.Backend
	for _, bcfg := range cfg.Backends {
		targetURL, _ := url.Parse(bcfg.URL)
		proxy := httputil.NewSingleHostReverseProxy(targetURL)

		// proxy optimization
		proxy.Transport = &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     60 * time.Second,
		}

		originalDirector := proxy.Director
		proxy.Director = func(req *http.Request) {
			originalDirector(req)
			req.Host = targetURL.Host
		}

		proxy.ErrorHandler = func(rw http.ResponseWriter, r *http.Request, e error) {
			log.Printf("backend error %s: %v", bcfg.URL, e)
			http.Error(rw, "backend error", http.StatusBadGateway)
		}

		backends = append(backends, &balancer.Backend{
			URL:               bcfg.URL,
			HealthEndpoint:    bcfg.HealthEndpoint,
			ActiveConnections: 0,
			Proxy:             proxy,
		})
	}

	lb := &balancer.LoadBalancer{
		Backends: backends,
	}

	// proxy server entrypoint
	http.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			cacheKEY := cache.CacheKey(request)

			// HIT
			if resp, isCached := cacheLRU.Get(cacheKEY); isCached {
				log.Printf("Cache HIT for %s", cacheKEY)
				for k, vv := range resp.Header {
					for _, v := range vv {
						writer.Header().Add(k, v)
					}
				}
				writer.WriteHeader(resp.StatusCode)
				writer.Write(resp.Body)
				return
			}

			backend, err := lb.GetLeastConnBackend()
			if err != nil {
				http.Error(writer, "no available backends", http.StatusServiceUnavailable)
				return
			}

			backend.IncConn()
			defer backend.DecConn()

			rec := httptest.NewRecorder()
			backend.Proxy.ServeHTTP(rec, request)

			body := rec.Body.Bytes()
			cached := &cache.CachedResponse{
				StatusCode: rec.Result().StatusCode,
				Header:     rec.Result().Header.Clone(),
				Body:       body,
				ExpiresAt:  time.Now().Add(cacheInvalidationTimer),
			}
			err = cacheLRU.Put(cacheKEY, cached)
			if err != nil {
				return
			}

			for k, vv := range rec.Header() {
				for _, v := range vv {
					writer.Header().Set(k, v)
				}
			}
			writer.WriteHeader(rec.Code)
			writer.Write(body)
			return
		}

		// (POST, PUT...) no cache
		backend, err := lb.GetLeastConnBackend()
		if err != nil {
			http.Error(writer, "no available backends", http.StatusServiceUnavailable)
			return
		}
		backend.IncConn()
		defer backend.DecConn()
		backend.Proxy.ServeHTTP(writer, request)
	})

	// healthcheck
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	healthcheck.StartHealthCheck(ctx, lb, healthcheckInterval)

	// logging periodico dei backend
	go func() {
		for {
			time.Sleep(time.Millisecond)
			printBackends(lb)
			time.Sleep(10 * time.Second)
		}
	}()

	log.Printf("Starting proxy server on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func printBackends(lb *balancer.LoadBalancer) {
	for _, backend := range lb.Backends {
		log.Printf("Backend → URL: %s, CONNECTIONS: %d, ALIVE: %t",
			backend.URL,
			backend.ActiveConnections,
			backend.IsAlive(),
		)
	}
}
