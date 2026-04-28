# eBPF / XDP Load Balancer

An experimental Layer-4 load balancer with a kernel-space eBPF/XDP data plane (C) and a concurrent user-space control plane (Go), exploring low-overhead packet steering, flow affinity, and runtime map synchronization.

The project separates the packet processing path in the Linux kernel (**Data Plane**) from configuration management, health checking, and telemetry in user space (**Control Plane**).

```
                            [ Ingress Traffic ]
                                     |
                                     v
       +-----------------------------------------------------------+
       | Linux Kernel Space (Ring 0)                               |
       |                                                           |
       |   +-----------------------+                               |
       |   | XDP Driver / Generic  |                               |
       |   |     (xdp_lb.bpf.c)    |                               |
       |   +-----------+-----------+                               |
       |               |                                           |
       |     5-tuple   v                                           |
       |    [ connection_table ]  (LRU Hash Map, 65k flows)        |
       |    - Match: reuse bound backend                           |
       |    - Miss:  FNV-1a hash (saddr, daddr, sport, dport, proto)|
       |               |                                           |
       |               v                                           |
       |       [ backends_map ]    (Array Map: IP, Port, MAC)      |
       |               |                                           |
       |               v                                           |
       |       D-NAT Rewrite + RFC 1624 Incremental Checksums      |
       |               |                                           |
       |       +-------+-------+                                   |
       |       |               |                                   |
       |       v               v                                   |
       |    [XDP_TX]       [XDP_PASS]                              |
       |  (same iface)   (to local stack)                          |
       +-------+---------------+-----------------------------------+
               |               |
               v               +-------------------+
      Forwarded to Backend                         |
                                                   v
       +-----------------------------------------------------------+
       | User Space Control Plane (Ring 3) - Go Daemon             |
       |                                                           |
       |   +---------------------+       +---------------------+   |
       |   | Active Healthchecks | ----> | cilium/ebpf Map Sync|   |
       |   |  (2-strike machine) |       | (runtime map update)|   |
       |   +---------------------+       +----------+----------+   |
       |                                            |              |
       |   +---------------------+                  v              |
       |   | Prometheus /metrics | <------- Scrapes Stats Map      |
       |   |  (per-CPU telemetry)|          (zero contention)      |
       |   +---------------------+                                 |
       +-----------------------------------------------------------+
```

---

## Why this architecture?

In standard user-space reverse proxies (such as Envoy, HAProxy, NGINX, or Go's standard reverse proxy), every packet traverses the full operating system networking stack:
1. The NIC raises hardware/software interrupts.
2. The kernel allocates an `sk_buff` data structure (hundreds of bytes of metadata).
3. The packet traverses IP routing, netfilter connection tracking, and socket queues.
4. The user-space daemon wakes up via `epoll`/syscalls, copying bytes between kernel and user memory.

For pure Layer-4 packet steering, this model introduces overhead from memory allocations, cache invalidation, and context switches.

**The eBPF/XDP approach:**
By running an XDP program directly inside the network driver before `sk_buff` allocation, packets are inspected and rewritten as early as possible. When a packet matches the configured Virtual IP (VIP), the destination L3/L4 headers are rewritten and the packet is retransmitted.

The Go daemon acts as a **Control Plane**: it probes upstream backend health, manages service topology, and updates kernel BPF maps at runtime using `cilium/ebpf`.

---

## Technical Internals

### 1. 5-Tuple Flow Hashing & LRU Session Affinity
Every connection is identified by its 5-tuple: `(Source IP, Destination IP, Source Port, Destination Port, Protocol)`.

When a packet arrives:
* The XDP program queries `connection_table` (a kernel `BPF_MAP_TYPE_LRU_HASH` with 65,536 entries).
* **Flow Hit**: If a flow entry exists and its backend is still active, the packet is forwarded to that backend, preserving TCP connection stickiness.
* **Flow Miss**: If it is a new connection or the assigned backend is dead, an FNV-1a hash is computed over the 5-tuple to select an active backend from `backends_map`.
* The new flow is stored in `connection_table`. If the table fills up, the kernel LRU logic evicts the least recently active connection.

> **Note on Hashing**: This program uses simple flow hashing with modulo backend selection, not consistent hashing (such as Google Maglev or Ketama). To prevent established connections from breaking when the backend pool changes, the system relies on the LRU connection table to cache existing flow assignments.

### 2. RFC 1624 Incremental Checksums
Recomputing IPv4 and TCP/UDP checksums from scratch across the entire packet on every forwarded packet is computationally expensive.

Instead, the program implements RFC 1624 incremental one's complement arithmetic:

```text
HC' = ~(~HC + ~m + m')
```

When changing destination IP from `m` to `m'` (and destination port), the checksum difference is computed in one's complement and the carry bits are folded back into 16 bits. This updates the checksum via a few arithmetic bitwise operations without reading the packet payload.

### 3. Runtime Map Synchronization
When an upstream backend fails health checks:
* The Go health checker detects the failure via active TCP/HTTP probes.
* It marks the backend inactive in the kernel `backends_map` (`active = 0`) via `bpf_map_update_elem`.
* Subsequent packet lookups in the kernel immediately bypass the inactive node.

### 4. Health Check Hysteresis
To prevent route flapping from transient latency spikes or dropped probes, the health checker implements a 2-strike state machine:
* A healthy backend is only marked `DOWN` after 2 consecutive failed probes.
* An inactive backend is only restored to `UP` after 2 consecutive successful probes.

### 5. Multi-Core Scaling
Telemetry is stored in a `BPF_MAP_TYPE_PERCPU_ARRAY`. Each CPU core updates its private counters without cross-core cache line bouncing or bus locks. The Go metrics collector periodically scrapes and aggregates these counters for Prometheus.

---

## Network Topology & Traffic Flow

Understanding how packets enter and leave this setup is critical:

### 1. Forward Path & One-Arm Topology (`XDP_TX`)
The XDP data plane forwards packets using the `XDP_TX` action. `XDP_TX` transmits the modified packet **out the same network interface on which it arrived**. This requires a **one-arm (single-interface / hairpin) topology**, where the load balancer and backends reside on the same broadcast domain or switch.

### 2. Return Path Considerations
With Layer-4 D-NAT, only the destination IP is rewritten to the backend IP. The source IP remains the original client IP:
* When the backend responds, its source IP is the backend IP. If this packet reaches the client directly, the client drops it because it expects packets from the VIP.
* In a production D-NAT topology, the backend's default gateway must route return traffic back through the load balancer, which performs reverse NAT before sending it to the client.
* Alternatively, in production environments with asymmetric traffic, **Direct Server Return (DSR)** with IP-in-IP encapsulation is used so backends reply directly with the VIP as source (see Limitations).

In the local automated test script (`scripts/test_netns.sh`), the setup runs in `dual` mode: XDP passes matching packets to the local network stack (`XDP_PASS`), and the Go user-space proxy handles bidirectional socket forwarding across virtual interfaces.

---

## Performance & Microbenchmarks

### User-Space Go Microbenchmarks
The microbenchmarks below measure the user-space Go components (the least-connections backend selection algorithm and the LRU response cache), not kernel XDP forwarding latency:

```text
pkg: internal/balancer
BenchmarkGetLeastConnBackend-20    29675389    45.48 ns/op     0 B/op    0 allocs/op

pkg: pkg/cache
BenchmarkLRUCache_GetPut-20         3571742   325.80 ns/op    22 B/op    1 allocs/op
```

* **Least-Connections Selection**: ~45 nanoseconds per lookup with zero heap allocations.
* **LRU Response Cache**: Concurrent read/write with lazy TTL eviction.

> **Data Plane Performance Note**: True line-rate forwarding and PPS (packets per second) throughput can only be accurately benchmarked on physical NICs with native driver support (`XDP_DRV`, e.g. Intel or Mellanox) using traffic generators like `pktgen` or TRex. On virtual `veth` pairs in `XDP_GENERIC` mode, packets still allocate `sk_buff` internally in the kernel.

---

## Project Structure

```
├── bpf/
│   ├── vmlinux.h               # Kernel BTF definitions (CO-RE enabled)
│   └── xdp_lb.bpf.c            # Data Plane: XDP driver hook, D-NAT, RFC 1624
├── cmd/
│   └── proxy/
│       ├── config.yaml         # Service VIP, port, and backend definitions
│       └── main.go             # Control Plane: CLI, signal handling, orchestration
├── internal/
│   ├── balancer/               # Least-Connections balancer & hysteresis logic
│   ├── config/                 # YAML configuration parser
│   ├── healthcheck/            # Concurrent active health checker
│   └── metrics/                # Prometheus exporter and eBPF scraper
├── pkg/
│   ├── bpf/                    # bpf2go bindings and XDP link manager
│   └── cache/                  # Concurrency-safe LRU cache with TTL
├── scripts/
│   └── test_netns.sh           # Automated multi-namespace integration test
└── Makefile                    # Build, test, and benchmark targets
```

---

## Building and Running

### Prerequisites
* Linux kernel `>= 5.15` (with BTF enabled at `/sys/kernel/btf/vmlinux`)
* `clang` and `llvm` (`>= 14`)
* `go` (`>= 1.24`)
* `bpftool`

```bash
# Compile eBPF C code and generate Go bindings via bpf2go
make generate

# Build the binary
make build

# Run unit tests with race detection and benchmarks
make test
make bench
```

### Running with Configuration
```bash
sudo ./bin/proxy --config cmd/proxy/config.yaml --iface eth0 --mode xdp
```

Available modes:
* `xdp`: Kernel-space Layer-4 D-NAT forwarding via XDP.
* `l7`: User-space HTTP reverse proxy with keep-alive connection pooling (`http.Transport`) and LRU caching.
* `dual`: XDP telemetry pass-through combined with user-space proxying.

---

## Automated Integration Test (Network Namespaces)

The forwarding and health checking pipeline can be verified locally without multi-NIC hardware using Linux network namespaces:

```bash
sudo ./scripts/test_netns.sh
```

The script sets up four isolated namespaces (`client-ns`, `lb-ns`, `be1-ns`, `be2-ns`), links them with `veth` pairs, runs the balancer, sends HTTP requests, and verifies failover behavior.

---

## Known Limitations & Design Tradeoffs

1. **IPv4 Only**: No support for IPv6, IP options, packet fragmentation, or VLAN tags.
2. **Static MAC Addressing**: Backend MAC addresses are configured statically in `config.yaml`. The XDP program does not perform dynamic ARP resolution or Linux FIB lookups (`bpf_fib_lookup`).
3. **Flow Hashing vs. Consistent Hashing**: Because simple modulo hashing is used on flow cache misses, scaling the backend set up or down remaps un-cached flows. Implementing a Maglev lookup table is a logical next step.
4. **LRU Table Eviction on Long Connections**: Under heavy connection volume, long-lived idle TCP flows evicted from the 65k-entry LRU table will be re-hashed on the next packet. If the backend pool has changed, this can direct non-SYN packets to a different backend, causing a TCP RST.
5. **Single-Interface Forwarding (`XDP_TX`)**: Forwarding across multiple distinct network interfaces is not supported by `XDP_TX` alone; it would require `XDP_REDIRECT` with a BPF `devmap`.
6. **No Connection Draining**: When a backend is marked inactive, existing flows are not drained gracefully; new packets for those flows are immediately dropped or re-hashed.
7. **Virtual Environment Testing**: Tested and benchmarked primarily using `XDP_GENERIC` on virtual `veth` interfaces. Testing on physical 10G/40G/100G NICs with native driver mode (`XDP_DRV`) remains future work.
