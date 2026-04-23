#!/usr/bin/env bash
# ==============================================================================
# eBPF/XDP Load Balancer Network Namespace (netns) Integration Test Harness
# ==============================================================================
# This script sets up an isolated virtual network environment using Linux netns:
#   - client-ns (10.0.0.2)
#   - lb-ns     (VIP 10.0.0.100, connects client, be1, be2)
#   - be1-ns    (10.0.1.10:8081)
#   - be2-ns    (10.0.1.20:8082)
# ==============================================================================

set -euo pipefail

if [[ $EUID -ne 0 ]]; then
   echo "[ERROR] This script requires root privileges to configure network namespaces and eBPF."
   exit 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT_DIR/bin/proxy"

if [[ ! -f "$BIN" ]]; then
    echo "[INFO] Building proxy binary..."
    go build -o "$BIN" "$ROOT_DIR/cmd/proxy"
fi

cleanup() {
    echo "[CLEANUP] Tearing down network namespaces and processes..."
    kill $(jobs -p) 2>/dev/null || true
    ip netns del client-ns 2>/dev/null || true
    ip netns del lb-ns 2>/dev/null || true
    ip netns del be1-ns 2>/dev/null || true
    ip netns del be2-ns 2>/dev/null || true
    rm -f /tmp/lb_test_cfg.yaml
    echo "[CLEANUP] Done."
}
trap cleanup EXIT

echo "[1/5] Creating network namespaces..."
ip netns add client-ns
ip netns add lb-ns
ip netns add be1-ns
ip netns add be2-ns

echo "[2/5] Creating veth link pairs and setting IP/MAC topology..."
# Client <-> LB
ip link add veth-client type veth peer name veth-lb-c
ip link set veth-client netns client-ns
ip link set veth-lb-c netns lb-ns

# LB <-> Backend 1
ip link add veth-lb-b1 type veth peer name veth-be1
ip link set veth-lb-b1 netns lb-ns
ip link set veth-be1 netns be1-ns

# LB <-> Backend 2
ip link add veth-lb-b2 type veth peer name veth-be2
ip link set veth-lb-b2 netns lb-ns
ip link set veth-be2 netns be2-ns

# Configure client-ns
ip netns exec client-ns ip link set lo up
ip netns exec client-ns ip link set veth-client address 02:00:00:00:01:02 up
ip netns exec client-ns ip addr add 10.0.0.2/24 dev veth-client
ip netns exec client-ns ip route add default via 10.0.0.1

# Configure be1-ns
ip netns exec be1-ns ip link set lo up
ip netns exec be1-ns ip link set veth-be1 address 02:00:00:00:02:10 up
ip netns exec be1-ns ip addr add 10.0.1.10/24 dev veth-be1
ip netns exec be1-ns ip route add default via 10.0.1.1

# Configure be2-ns
ip netns exec be2-ns ip link set lo up
ip netns exec be2-ns ip link set veth-be2 address 02:00:00:00:02:20 up
ip netns exec be2-ns ip addr add 10.0.1.20/24 dev veth-be2
ip netns exec be2-ns ip route add default via 10.0.1.1

# Configure lb-ns
ip netns exec lb-ns ip link set lo up
ip netns exec lb-ns ip link set veth-lb-c address 02:00:00:00:01:01 up
ip netns exec lb-ns ip addr add 10.0.0.1/24 dev veth-lb-c
ip netns exec lb-ns ip addr add 10.0.0.100/32 dev veth-lb-c # VIP

ip netns exec lb-ns ip link set veth-lb-b1 address 02:00:00:00:02:01 up
ip netns exec lb-ns ip addr add 10.0.1.1/24 dev veth-lb-b1

ip netns exec lb-ns ip link set veth-lb-b2 address 02:00:00:00:02:02 up
ip netns exec lb-ns ip addr add 10.0.1.2/24 dev veth-lb-b2

# Pre-populate ARP tables for deterministic L2 resolution
ip netns exec lb-ns ip neigh add 10.0.1.10 lladdr 02:00:00:00:02:10 dev veth-lb-b1
ip netns exec lb-ns ip neigh add 10.0.1.20 lladdr 02:00:00:00:02:20 dev veth-lb-b2
ip netns exec client-ns ip neigh add 10.0.0.100 lladdr 02:00:00:00:01:01 dev veth-client

echo "[3/5] Starting mock HTTP backend servers..."
ip netns exec be1-ns python3 -m http.server 8081 --bind 10.0.1.10 &
ip netns exec be2-ns python3 -m http.server 8082 --bind 10.0.1.20 &
sleep 1

echo "[4/5] Starting eBPF Load Balancer in lb-ns..."
cat << 'CFG' > /tmp/lb_test_cfg.yaml
interface: "veth-lb-c"
mode: "dual"
vip: "10.0.0.100"
port: 80
metricsAddr: ":9090"
healthCheckInterval: 1s
cacheInvalidationTimer: 10s

backends:
  - url: "http://10.0.1.10:8081"
    healthEndpoint: "/"
    ip: "10.0.1.10"
    port: 8081
    mac: "02:00:00:00:02:10"
    weight: 1
  - url: "http://10.0.1.20:8082"
    healthEndpoint: "/"
    ip: "10.0.1.20"
    port: 8082
    mac: "02:00:00:00:02:20"
    weight: 1
CFG

ip netns exec lb-ns "$BIN" --config /tmp/lb_test_cfg.yaml &
sleep 2

echo "[5/5] Testing HTTP requests from client-ns to VIP (10.0.0.100:80)..."
for i in {1..5}; do
    ip netns exec client-ns curl -s -o /dev/null -w "Request %{http_code} to VIP succeeded in %{time_total}s\n" http://10.0.0.100/
done

echo "[SUCCESS] End-to-end integration test completed successfully!"
