package bpf

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"

	"github.com/cilium/ebpf/link"
)

// ForwardMode defines how XDP treats matching packets.
type ForwardMode uint32

const (
	ForwardModeTX   ForwardMode = 0 // XDP_TX: Fast-path L4 D-NAT forwarding
	ForwardModePass ForwardMode = 1 // XDP_PASS: Kernel stack pass-through (telemetry mode)
)

// AttachMode defines the XDP hook driver mode.
type AttachMode int

const (
	AttachModeGeneric AttachMode = 1
	AttachModeDriver  AttachMode = 2
)

// BackendEntry represents backend server metadata stored in the kernel.
type BackendEntry struct {
	IP     net.IP
	Port   uint16
	MAC    net.HardwareAddr
	Weight uint32
	Active bool
}

// Stats aggregates per-CPU packet and byte metrics from the kernel.
type Stats struct {
	RxPackets        uint64
	RxBytes          uint64
	ForwardedPackets uint64
	DroppedPackets   uint64
	PassPackets      uint64
}

// Manager controls the lifecycle of eBPF programs, maps, and network links.
type Manager struct {
	objs xdpLbObjects
	link link.Link
	mu   sync.RWMutex
}

// NewManager loads the compiled eBPF objects into the Linux kernel.
func NewManager() (*Manager, error) {
	var objs xdpLbObjects
	if err := loadXdpLbObjects(&objs, nil); err != nil {
		return nil, fmt.Errorf("failed to load BPF objects: %w", err)
	}
	return &Manager{objs: objs}, nil
}

// Attach attaches the XDP program to the designated network interface.
func (m *Manager) Attach(ifaceName string, mode AttachMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return fmt.Errorf("interface %s not found: %w", ifaceName, err)
	}

	flags := link.XDPGenericMode
	if mode == AttachModeDriver {
		flags = link.XDPDriverMode
	}

	l, err := link.AttachXDP(link.XDPOptions{
		Program:   m.objs.XdpLbFunc,
		Interface: iface.Index,
		Flags:     flags,
	})
	if err != nil {
		return fmt.Errorf("failed to attach XDP to %s: %w", ifaceName, err)
	}

	m.link = l
	return nil
}

// ConfigureService updates the service VIP and service port in the kernel map.
func (m *Manager) ConfigureService(vip net.IP, port uint16, numBackends uint16, fwdMode ForwardMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	v4 := vip.To4()
	if v4 == nil {
		return fmt.Errorf("VIP must be an IPv4 address, got %s", vip)
	}

	cfg := xdpLbServiceConfig{
		Vip:         binary.BigEndian.Uint32(v4),
		Port:        htons(port),
		NumBackends: numBackends,
		Enabled:     1,
		ForwardMode: uint32(fwdMode),
	}

	key := uint32(0)
	if err := m.objs.ServiceMap.Put(&key, &cfg); err != nil {
		return fmt.Errorf("failed to update service map: %w", err)
	}
	return nil
}

// SetBackend writes or updates a backend entry at the given index in backends_map.
func (m *Manager) SetBackend(idx uint32, be BackendEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	v4 := be.IP.To4()
	if v4 == nil {
		return fmt.Errorf("backend IP must be IPv4: %s", be.IP)
	}

	var macBytes [6]uint8
	if len(be.MAC) == 6 {
		copy(macBytes[:], be.MAC)
	}

	activeVal := uint32(0)
	if be.Active {
		activeVal = 1
	}

	info := xdpLbBackendInfo{
		Ipv4:   binary.BigEndian.Uint32(v4),
		Port:   htons(be.Port),
		Mac:    macBytes,
		Weight: be.Weight,
		Active: activeVal,
	}

	if err := m.objs.BackendsMap.Put(&idx, &info); err != nil {
		return fmt.Errorf("failed to put backend at index %d: %w", idx, err)
	}
	return nil
}

// SetBackendActive updates the health status of a backend atomically.
func (m *Manager) SetBackendActive(idx uint32, active bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var info xdpLbBackendInfo
	if err := m.objs.BackendsMap.Lookup(&idx, &info); err != nil {
		return fmt.Errorf("failed to lookup backend %d: %w", idx, err)
	}

	if active {
		info.Active = 1
	} else {
		info.Active = 0
	}

	if err := m.objs.BackendsMap.Put(&idx, &info); err != nil {
		return fmt.Errorf("failed to update backend status for index %d: %w", idx, err)
	}
	return nil
}

// GetStats aggregates per-CPU packet counters from the kernel stats map.
func (m *Manager) GetStats() (Stats, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var perCPUStats []xdpLbStatsRecord
	key := uint32(0)
	if err := m.objs.StatsMap.Lookup(&key, &perCPUStats); err != nil {
		return Stats{}, fmt.Errorf("failed to lookup stats map: %w", err)
	}

	var total Stats
	for _, s := range perCPUStats {
		total.RxPackets += s.RxPackets
		total.RxBytes += s.RxBytes
		total.ForwardedPackets += s.ForwardedPackets
		total.DroppedPackets += s.DroppedPackets
		total.PassPackets += s.PassPackets
	}
	return total, nil
}

// Close detaches the XDP hook and releases kernel resources.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var firstErr error
	if m.link != nil {
		if err := m.link.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := m.objs.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func htons(v uint16) uint16 {
	return (v << 8) | (v >> 8)
}
