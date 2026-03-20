//go:build ignore

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

char __license[] SEC("license") = "Dual MIT/GPL";

#define MAX_BACKENDS 64
#define ETH_ALEN 6

#ifndef ETH_P_IP
#define ETH_P_IP 0x0800
#endif

// backend_info describes a destination backend server
struct backend_info {
    __u32 ipv4;            // Backend IPv4 in network byte order
    __u16 port;            // Backend port in network byte order
    __u8  mac[ETH_ALEN];   // Backend MAC address
    __u32 weight;          // Load balancing weight
    __u32 active;          // 1 = healthy, 0 = unhealthy
};

// service_config describes the virtual IP service
struct service_config {
    __u32 vip;             // VIP in network byte order
    __u16 port;            // Service port in network byte order
    __u16 num_backends;    // Total configured backends
    __u32 enabled;         // 1 = enabled, 0 = disabled
};

// stats_record per-cpu telemetry
struct stats_record {
    __u64 rx_packets;
    __u64 rx_bytes;
    __u64 forwarded_packets;
    __u64 dropped_packets;
    __u64 pass_packets;
};

// BPF Maps
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __type(key, __u32);
    __type(value, struct service_config);
    __uint(max_entries, 1);
} service_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __type(key, __u32);
    __type(value, struct backend_info);
    __uint(max_entries, MAX_BACKENDS);
} backends_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __type(key, __u32);
    __type(value, struct stats_record);
    __uint(max_entries, 1);
} stats_map SEC(".maps");

static __always_inline void update_stats(__u64 bytes, int action) {
    __u32 key = 0;
    struct stats_record *rec = bpf_map_lookup_elem(&stats_map, &key);
    if (!rec)
        return;
    rec->rx_packets++;
    rec->rx_bytes += bytes;
    if (action == XDP_TX) {
        rec->forwarded_packets++;
    } else if (action == XDP_DROP) {
        rec->dropped_packets++;
    } else {
        rec->pass_packets++;
    }
}

SEC("xdp")
int xdp_lb_func(struct xdp_md *ctx) {
    void *data = (void *)(long)ctx->data;
    void *data_end = (void *)(long)ctx->data_end;
    __u64 pkt_len = data_end - data;

    // Parse Ethernet header
    struct ethhdr *eth = data;
    if ((void *)(eth + 1) > data_end) {
        return XDP_PASS;
    }

    if (eth->h_proto != bpf_htons(ETH_P_IP)) {
        return XDP_PASS;
    }

    // Parse IPv4 header
    struct iphdr *iph = (void *)(eth + 1);
    if ((void *)(iph + 1) > data_end) {
        return XDP_PASS;
    }

    if (iph->ihl < 5) {
        return XDP_PASS;
    }
    void *l4_hdr = (void *)iph + (iph->ihl * 4);
    if (l4_hdr > data_end) {
        return XDP_PASS;
    }

    // Check service configuration
    __u32 cfg_key = 0;
    struct service_config *cfg = bpf_map_lookup_elem(&service_map, &cfg_key);
    if (!cfg || !cfg->enabled) {
        return XDP_PASS;
    }

    // Check destination IP matches VIP
    if (iph->daddr != cfg->vip) {
        return XDP_PASS;
    }

    // Check L4 protocol (TCP or UDP)
    __u16 dport = 0;
    if (iph->protocol == IPPROTO_TCP) {
        struct tcphdr *tcph = l4_hdr;
        if ((void *)(tcph + 1) > data_end) {
            return XDP_PASS;
        }
        dport = tcph->dest;
    } else if (iph->protocol == IPPROTO_UDP) {
        struct udphdr *udph = l4_hdr;
        if ((void *)(udph + 1) > data_end) {
            return XDP_PASS;
        }
        dport = udph->dest;
    } else {
        return XDP_PASS;
    }

    // Verify service port
    if (dport != cfg->port) {
        return XDP_PASS;
    }

    update_stats(pkt_len, XDP_PASS);
    return XDP_PASS;
}
