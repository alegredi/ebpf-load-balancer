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
    __u32 forward_mode;    // 0 = XDP_TX (D-NAT fast path), 1 = XDP_PASS (telemetry mode)
};

// flow_key represents the 5-tuple identifying a network flow
struct flow_key {
    __u32 src_ip;
    __u32 dst_ip;
    __u16 src_port;
    __u16 dst_port;
    __u8  protocol;
    __u8  _pad[3];
};

// flow_value tracks session affinity
struct flow_value {
    __u32 backend_index;
    __u64 created_at_ns;
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
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __type(key, struct flow_key);
    __type(value, struct flow_value);
    __uint(max_entries, 65536);
} connection_table SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __type(key, __u32);
    __type(value, struct stats_record);
    __uint(max_entries, 1);
} stats_map SEC(".maps");

// RFC 1624 incremental checksum update for 32-bit values
static __always_inline void csum_replace4(__u16 *csum, __u32 from, __u32 to) {
    __u32 hc = ~(*csum) & 0xffff;
    __u32 m_old = (~from & 0xffff) + (~(from >> 16) & 0xffff);
    __u32 m_new = (to & 0xffff) + (to >> 16);
    __u32 sum = hc + m_old + m_new;
    while (sum >> 16) {
        sum = (sum & 0xffff) + (sum >> 16);
    }
    *csum = (__u16)(~sum);
}

// RFC 1624 incremental checksum update for 16-bit values
static __always_inline void csum_replace2(__u16 *csum, __u16 from, __u16 to) {
    __u32 hc = ~(*csum) & 0xffff;
    __u32 m_old = ~from & 0xffff;
    __u32 m_new = to & 0xffff;
    __u32 sum = hc + m_old + m_new;
    while (sum >> 16) {
        sum = (sum & 0xffff) + (sum >> 16);
    }
    *csum = (__u16)(~sum);
}

// 5-tuple consistent flow hash
static __always_inline __u32 hash_5tuple(const struct flow_key *key) {
    __u32 hash = 2166136261u;
    hash = (hash ^ key->src_ip) * 16777619u;
    hash = (hash ^ key->dst_ip) * 16777619u;
    hash = (hash ^ (__u32)key->src_port) * 16777619u;
    hash = (hash ^ ((__u32)key->dst_port << 16)) * 16777619u;
    hash = (hash ^ (__u32)key->protocol) * 16777619u;
    return hash;
}

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
    struct flow_key key = {0};
    key.src_ip = iph->saddr;
    key.dst_ip = iph->daddr;
    key.protocol = iph->protocol;

    struct tcphdr *tcph = NULL;
    struct udphdr *udph = NULL;

    if (iph->protocol == IPPROTO_TCP) {
        tcph = l4_hdr;
        if ((void *)(tcph + 1) > data_end) {
            return XDP_PASS;
        }
        key.src_port = tcph->source;
        key.dst_port = tcph->dest;
    } else if (iph->protocol == IPPROTO_UDP) {
        udph = l4_hdr;
        if ((void *)(udph + 1) > data_end) {
            return XDP_PASS;
        }
        key.src_port = udph->source;
        key.dst_port = udph->dest;
    } else {
        return XDP_PASS;
    }

    // Verify service port
    if (key.dst_port != cfg->port) {
        return XDP_PASS;
    }

    if (cfg->num_backends == 0 || cfg->num_backends > MAX_BACKENDS) {
        update_stats(pkt_len, XDP_DROP);
        return XDP_DROP;
    }

    // Check flow table for session affinity
    __u32 chosen_idx = 0;
    bool found_active = false;
    struct flow_value *flow = bpf_map_lookup_elem(&connection_table, &key);
    if (flow) {
        __u32 idx = flow->backend_index;
        if (idx < cfg->num_backends) {
            struct backend_info *be = bpf_map_lookup_elem(&backends_map, &idx);
            if (be && be->active) {
                chosen_idx = idx;
                found_active = true;
            }
        }
    }

    // If no existing healthy flow session, pick backend via 5-tuple hash
    if (!found_active) {
        __u32 hash = hash_5tuple(&key);
        __u32 candidate = hash % cfg->num_backends;

        #pragma unroll
        for (int i = 0; i < MAX_BACKENDS; i++) {
            if (i >= cfg->num_backends)
                break;
            __u32 idx = (candidate + i) % cfg->num_backends;
            struct backend_info *be = bpf_map_lookup_elem(&backends_map, &idx);
            if (be && be->active) {
                chosen_idx = idx;
                found_active = true;
                break;
            }
        }
    }

    if (!found_active) {
        update_stats(pkt_len, XDP_DROP);
        return XDP_DROP;
    }

    // Update connection table for session affinity
    struct flow_value new_flow = {
        .backend_index = chosen_idx,
        .created_at_ns = bpf_ktime_get_ns(),
    };
    bpf_map_update_elem(&connection_table, &key, &new_flow, BPF_ANY);

    struct backend_info *target = bpf_map_lookup_elem(&backends_map, &chosen_idx);
    if (!target) {
        update_stats(pkt_len, XDP_DROP);
        return XDP_DROP;
    }

    // If configured in PASS/telemetry mode, do not rewrite packet, just record telemetry
    if (cfg->forward_mode == 1) {
        update_stats(pkt_len, XDP_PASS);
        return XDP_PASS;
    }

    // Perform L4 D-NAT rewrite
    __u32 old_daddr = iph->daddr;
    __u32 new_daddr = target->ipv4;
    iph->daddr = new_daddr;
    csum_replace4(&iph->check, old_daddr, new_daddr);

    if (tcph) {
        __u16 old_dport = tcph->dest;
        __u16 new_dport = target->port;
        tcph->dest = new_dport;
        csum_replace4(&tcph->check, old_daddr, new_daddr);
        csum_replace2(&tcph->check, old_dport, new_dport);
    } else if (udph) {
        __u16 old_dport = udph->dest;
        __u16 new_dport = target->port;
        udph->dest = new_dport;
        if (udph->check != 0) {
            csum_replace4(&udph->check, old_daddr, new_daddr);
            csum_replace2(&udph->check, old_dport, new_dport);
            if (udph->check == 0)
                udph->check = 0xffff;
        }
    }

    // Rewrite L2 Ethernet addresses:
    // Destination MAC becomes backend MAC, Source MAC becomes LB MAC (current dest MAC)
    __u8 old_dst_mac[ETH_ALEN];
    __builtin_memcpy(old_dst_mac, eth->h_dest, ETH_ALEN);
    __builtin_memcpy(eth->h_dest, target->mac, ETH_ALEN);
    __builtin_memcpy(eth->h_source, old_dst_mac, ETH_ALEN);

    update_stats(pkt_len, XDP_TX);
    return XDP_TX;
}
