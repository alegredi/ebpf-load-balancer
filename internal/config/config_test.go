package config

import (
	"os"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	yamlData := `
interface: "eth0"
mode: "xdp"
vip: "192.168.1.100"
port: 80
metricsAddr: ":9100"
healthCheckInterval: "2s"
backends:
  - url: "http://192.168.1.10:8080"
    healthEndpoint: "/healthz"
    mac: "00:11:22:33:44:55"
    weight: 3
`
	tmpFile, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write([]byte(yamlData)); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	tmpFile.Close()

	cfg, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("unexpected error loading config: %v", err)
	}

	if cfg.Interface != "eth0" {
		t.Errorf("expected interface eth0, got %s", cfg.Interface)
	}
	if cfg.Mode != "xdp" {
		t.Errorf("expected mode xdp, got %s", cfg.Mode)
	}
	if cfg.VIP != "192.168.1.100" {
		t.Errorf("expected vip 192.168.1.100, got %s", cfg.VIP)
	}
	if cfg.Port != 80 {
		t.Errorf("expected port 80, got %d", cfg.Port)
	}
	if len(cfg.Backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(cfg.Backends))
	}
	if cfg.Backends[0].IP != "192.168.1.10" {
		t.Errorf("expected inferred IP 192.168.1.10, got %s", cfg.Backends[0].IP)
	}
	if cfg.Backends[0].Port != 8080 {
		t.Errorf("expected inferred port 8080, got %d", cfg.Backends[0].Port)
	}
	if cfg.Backends[0].Weight != 3 {
		t.Errorf("expected weight 3, got %d", cfg.Backends[0].Weight)
	}
}
