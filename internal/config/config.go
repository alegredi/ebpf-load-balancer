package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type BackendConfig struct {
	URL            string `yaml:"url"`
	HealthEndpoint string `yaml:"healthEndpoint"`
	IP             string `yaml:"ip"`
	Port           uint16 `yaml:"port"`
	MAC            string `yaml:"mac"`
	Weight         uint32 `yaml:"weight"`
}

type Config struct {
	Interface              string          `yaml:"interface"`
	Mode                   string          `yaml:"mode"` // "xdp", "l7", or "dual"
	VIP                    string          `yaml:"vip"`
	Port                   uint16          `yaml:"port"`
	MetricsAddr            string          `yaml:"metricsAddr"`
	HealthCheckInterval    string          `yaml:"healthCheckInterval"`
	CacheInvalidationTimer string          `yaml:"cacheInvalidationTimer"`
	Backends               []BackendConfig `yaml:"backends"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling YAML config: %w", err)
	}

	// Apply defaults
	if cfg.Mode == "" {
		cfg.Mode = "dual"
	}
	if cfg.VIP == "" {
		cfg.VIP = "127.0.0.1"
	}
	if cfg.Port == 0 {
		cfg.Port = 8080
	}
	if cfg.MetricsAddr == "" {
		cfg.MetricsAddr = ":9090"
	}
	if cfg.HealthCheckInterval == "" {
		cfg.HealthCheckInterval = "5s"
	}
	if cfg.CacheInvalidationTimer == "" {
		cfg.CacheInvalidationTimer = "30s"
	}

	// Infer IP and Port from URL if not explicitly supplied
	for i := range cfg.Backends {
		b := &cfg.Backends[i]
		if b.Weight == 0 {
			b.Weight = 1
		}
		if b.IP == "" || b.Port == 0 {
			if u, err := url.Parse(b.URL); err == nil {
				host, portStr, err := net.SplitHostPort(u.Host)
				if err == nil {
					if b.IP == "" {
						if host == "localhost" {
							b.IP = "127.0.0.1"
						} else {
							b.IP = host
						}
					}
					if b.Port == 0 {
						if p, err := strconv.Atoi(portStr); err == nil {
							b.Port = uint16(p)
						}
					}
				}
			}
		}
	}

	return &cfg, nil
}

func (c *Config) HealthCheckDuration() time.Duration {
	d, err := time.ParseDuration(c.HealthCheckInterval)
	if err != nil {
		return 5 * time.Second
	}
	return d
}
