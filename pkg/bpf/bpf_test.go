package bpf

import (
	"testing"

	"github.com/cilium/ebpf"
)

func TestLoadCollectionSpec(t *testing.T) {
	spec, err := loadXdpLb()
	if err != nil {
		t.Fatalf("failed to load collection spec: %v", err)
	}

	if spec.Maps["service_map"] == nil {
		t.Errorf("expected map service_map to exist")
	} else if spec.Maps["service_map"].Type != ebpf.Array {
		t.Errorf("expected service_map to be Array, got %v", spec.Maps["service_map"].Type)
	}

	if spec.Maps["backends_map"] == nil {
		t.Errorf("expected map backends_map to exist")
	} else if spec.Maps["backends_map"].MaxEntries != 64 {
		t.Errorf("expected backends_map max entries 64, got %d", spec.Maps["backends_map"].MaxEntries)
	}

	if spec.Maps["connection_table"] == nil {
		t.Errorf("expected map connection_table to exist")
	} else if spec.Maps["connection_table"].Type != ebpf.LRUHash {
		t.Errorf("expected connection_table to be LRUHash, got %v", spec.Maps["connection_table"].Type)
	}

	if spec.Maps["stats_map"] == nil {
		t.Errorf("expected map stats_map to exist")
	} else if spec.Maps["stats_map"].Type != ebpf.PerCPUArray {
		t.Errorf("expected stats_map to be PerCPUArray, got %v", spec.Maps["stats_map"].Type)
	}

	if spec.Programs["xdp_lb_func"] == nil {
		t.Fatalf("expected program xdp_lb_func to exist")
	}
	if spec.Programs["xdp_lb_func"].SectionName != "xdp" {
		t.Errorf("expected section name 'xdp', got %s", spec.Programs["xdp_lb_func"].SectionName)
	}
}
