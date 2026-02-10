package config

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type BackendConfig struct {
	URL            string `yaml:"url"`
	HealthEndpoint string `yaml:"healthEndpoint"`
}

type Config struct {
	HealthCheckInterval    string          `yaml:"healthCheckInterval"`
	CacheInvalidationTimer string          `yaml:"cacheInvalidationTimer"`
	Backends               []BackendConfig `yaml:"backends"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) HealthCheckDuration() time.Duration {
	d, err := time.ParseDuration(c.HealthCheckInterval)
	if err != nil {
		return 5 * time.Second // default
	}
	return d
}
