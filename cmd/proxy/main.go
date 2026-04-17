package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"proxy/internal/balancer"
	"proxy/internal/config"
	"proxy/internal/healthcheck"
	"proxy/internal/metrics"
	"proxy/pkg/bpf"
	"proxy/pkg/cache"
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
	configPath := flag.String("config", "config.yaml", "Path to YAML configuration file")
	modeFlag := flag.String("mode", "", "Execution mode: xdp, l7, or dual (overrides config)")
	ifaceFlag := flag.String("iface", "", "Network interface for XDP (overrides config)")
	vipFlag := flag.String("vip", "", "Virtual IP address (overrides config)")
	portFlag := flag.Int("port", 0, "Service port (overrides config)")
	metricsAddrFlag := flag.String("metrics-addr", "", "Prometheus metrics address (overrides config)")
	flag.Parse()

	log.SetOutput(os.Stdout)
	log.Println("[INFO] ===============================================")
	log.Println("[INFO]  eBPF/XDP High-Performance Load Balancer (Go)  ")
	log.Println("[INFO] ===============================================")

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("[FATAL] Cannot load config from %s: %v", *configPath, err)
	}

	if *modeFlag != "" {
		cfg.Mode = *modeFlag
	}
	if *ifaceFlag != "" {
		cfg.Interface = *ifaceFlag
	}
	if *vipFlag != "" {
		cfg.VIP = *vipFlag
	}
	if *portFlag != 0 {
		cfg.Port = uint16(*portFlag)
	}
	if *metricsAddrFlag != "" {
		cfg.MetricsAddr = *metricsAddrFlag
	}

	log.Printf("[INFO] Mode: %s | VIP: %s:%d | Interface: %s | Metrics: %s",
		cfg.Mode, cfg.VIP, cfg.Port, cfg.Interface, cfg.MetricsAddr)

	cacheTTL, err := time.ParseDuration(cfg.CacheInvalidationTimer)
	if err != nil {
		cacheTTL = 30 * time.Second
	}
	cacheLRU := cache.NewLRUCache(512)

	// Build balancer backends
	var backends []*balancer.Backend
	for i, bcfg := range cfg.Backends {
		targetURL, err := url.Parse(bcfg.URL)
		if err != nil {
			log.Fatalf("[FATAL] Invalid backend URL %s: %v", bcfg.URL, err)
		}

		proxy := httputil.NewSingleHostReverseProxy(targetURL)
		proxy.Transport = &http.Transport{
			MaxIdleConns:          512,
			MaxIdleConnsPerHost:   128,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		}

		origDirector := proxy.Director
		proxy.Director = func(req *http.Request) {
			origDirector(req)
			req.Host = targetURL.Host
		}

		proxy.ErrorHandler = func(rw http.ResponseWriter, r *http.Request, e error) {
			log.Printf("[ERROR] Backend %s error: %v", bcfg.URL, e)
			http.Error(rw, "bad gateway", http.StatusBadGateway)
		}

		be := &balancer.Backend{
			Index:          uint32(i),
			URL:            bcfg.URL,
			HealthEndpoint: bcfg.HealthEndpoint,
			Weight:         bcfg.Weight,
			Proxy:          proxy,
		}
		be.SetAlive(true) // assume healthy initially until probed
		backends = append(backends, be)
	}

	lb := &balancer.LoadBalancer{
		Backends: backends,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize eBPF Manager if running in XDP or dual mode
	var bpfMgr *bpf.Manager
	if cfg.Mode == "xdp" || cfg.Mode == "dual" {
		log.Printf("[INFO] Initializing eBPF/XDP Kernel Data Plane...")
		mgr, err := bpf.NewManager()
		if err != nil {
			log.Printf("[WARN] Failed to load eBPF objects into kernel: %v", err)
			log.Printf("[WARN] Running in fallback user-space mode only (check sudo / CAP_BPF / CAP_NET_ADMIN privileges).")
		} else {
			bpfMgr = mgr
			defer bpfMgr.Close()

			// Configure Service
			vipIP := net.ParseIP(cfg.VIP)
			if vipIP == nil {
				vipIP = net.ParseIP("127.0.0.1")
			}

			fwdMode := bpf.ForwardModeTX
			if cfg.Mode == "dual" {
				fwdMode = bpf.ForwardModePass // passes to local stack in dual mode
			}

			if err := bpfMgr.ConfigureService(vipIP, cfg.Port, uint16(len(backends)), fwdMode); err != nil {
				log.Printf("[WARN] Failed to configure BPF service map: %v", err)
			}

			// Populate Backends Map
			for i, bcfg := range cfg.Backends {
				mac, _ := net.ParseMAC(bcfg.MAC)
				if mac == nil {
					mac = net.HardwareAddr{0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
				}
				beIP := net.ParseIP(bcfg.IP)
				if beIP == nil {
					beIP = net.ParseIP("127.0.0.1")
				}
				_ = bpfMgr.SetBackend(uint32(i), bpf.BackendEntry{
					IP:     beIP,
					Port:   bcfg.Port,
					MAC:    mac,
					Weight: bcfg.Weight,
					Active: true,
				})
			}

			// Attach XDP Hook
			if cfg.Interface != "" {
				if err := bpfMgr.Attach(cfg.Interface, bpf.AttachModeGeneric); err != nil {
					log.Printf("[WARN] Failed to attach XDP to interface %s: %v", cfg.Interface, err)
				} else {
					log.Printf("[INFO] Successfully attached XDP program to %s", cfg.Interface)
				}
			}

			metrics.StartTelemetryCollector(ctx, bpfMgr, 1*time.Second)
		}
	}

	// Start Prometheus Metrics Exporter
	metricsServer := metrics.StartMetricsServer(cfg.MetricsAddr)
	defer metricsServer.Close()

	// Start Health Checking with BPF map synchronization
	healthcheck.StartHealthCheck(ctx, lb, cfg.HealthCheckDuration(), func(b *balancer.Backend, alive bool) {
		statusVal := 0.0
		if alive {
			statusVal = 1.0
			log.Printf("[HEALTH] Backend %s [%d] is UP", b.URL, b.Index)
		} else {
			log.Printf("[HEALTH] Backend %s [%d] is DOWN", b.URL, b.Index)
			metrics.HealthcheckFailuresTotal.WithLabelValues(b.URL).Inc()
		}
		metrics.BackendHealthy.WithLabelValues(b.URL).Set(statusVal)

		if bpfMgr != nil {
			if err := bpfMgr.SetBackendActive(b.Index, alive); err != nil {
				log.Printf("[WARN] Failed to update BPF map for backend %d: %v", b.Index, err)
			}
		}
	})

	// Setup L7 HTTP Reverse Proxy (active in 'l7' and 'dual' modes)
	var l7Server *http.Server
	if cfg.Mode == "l7" || cfg.Mode == "dual" {
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
				metrics.BackendActiveConnections.WithLabelValues(backend.URL).Set(float64(backend.GetActiveConnections()))
				defer func() {
					backend.DecConn()
					metrics.BackendActiveConnections.WithLabelValues(backend.URL).Set(float64(backend.GetActiveConnections()))
				}()

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
						ExpiresAt:  time.Now().Add(cacheTTL),
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
			metrics.BackendActiveConnections.WithLabelValues(backend.URL).Set(float64(backend.GetActiveConnections()))
			defer func() {
				backend.DecConn()
				metrics.BackendActiveConnections.WithLabelValues(backend.URL).Set(float64(backend.GetActiveConnections()))
			}()
			backend.Proxy.ServeHTTP(w, r)
		})

		listenAddr := fmt.Sprintf(":%d", cfg.Port)
		l7Server = &http.Server{
			Addr:         listenAddr,
			Handler:      mux,
			ReadTimeout:  15 * time.Second,
			WriteTimeout: 15 * time.Second,
			IdleTimeout:  60 * time.Second,
		}

		go func() {
			log.Printf("[INFO] L7 Reverse Proxy listening on %s", listenAddr)
			if err := l7Server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("[FATAL] L7 server failed: %v", err)
			}
		}()
	}

	// Status logger
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, b := range lb.Backends {
					metrics.BackendActiveConnections.WithLabelValues(b.URL).Set(float64(b.GetActiveConnections()))
					log.Printf("[STATUS] Backend: %s [%d] | ActiveConns: %d | Alive: %t",
						b.URL, b.Index, b.GetActiveConnections(), b.IsAlive())
				}
				if bpfMgr != nil {
					if s, err := bpfMgr.GetStats(); err == nil {
						log.Printf("[XDP STATS] RxPkts: %d | FwdPkts: %d | DropPkts: %d | PassPkts: %d | Bytes: %d",
							s.RxPackets, s.ForwardedPackets, s.DroppedPackets, s.PassPackets, s.RxBytes)
					}
				}
			}
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	log.Println("[INFO] Received termination signal, starting graceful shutdown...")
	cancel()

	if l7Server != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := l7Server.Shutdown(shutdownCtx); err != nil {
			log.Printf("[ERROR] L7 Server forced shutdown: %v", err)
		}
	}

	if bpfMgr != nil {
		log.Println("[INFO] Detaching XDP hook and closing BPF maps...")
		if err := bpfMgr.Close(); err != nil {
			log.Printf("[WARN] Error closing BPF manager: %v", err)
		}
	}

	log.Println("[INFO] Load Balancer shutdown complete.")
}
