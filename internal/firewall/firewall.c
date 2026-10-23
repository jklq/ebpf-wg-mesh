//go:build ignore

#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/if_packet.h>
#include <linux/in.h>
#include <linux/ip.h>
#include <linux/ipv6.h>
#include <linux/pkt_cls.h>
#include <linux/tcp.h>
#include <linux/udp.h>

#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>

#define ROLE_WIREGUARD 1
#define ROLE_CONTAINER 2

struct connection_key {
    __u8 src_ip[16];
    __u8 dst_ip[16];
    __be16 src_port;
    __be16 dst_port;
    __u8 protocol;
    __u8 _pad1;
    __u16 _pad2;
};

struct identity_key {
    __u32 prefixlen;
    __u8 ip_address[16];
};

struct identity_value {
    __u32 network_identity;
    __u8 host_ip[16];
    __u32 veth_ifindex;
};

struct host_ip_value {
    __u8 ip[16];
};

struct container_policy {
    __u32 network_identity;
    __u8 ipv4[4];
    __u8 ipv6[16];
    /* Egress counter token: the task cgroup ID. Userspace creates the
     * counter entry at registration, so it is never evicted. */
    __u64 usage_token;
};

/* Conntrack value. pending_bytes holds control-only output bytes of an
 * ingress flow until the flow carries payload (public ingress) or ends
 * without payload (a platform probe). Evicting a flow loses its pending
 * bytes: a bounded undercount. */
#define FLOW_INGRESS 1
#define FLOW_PAYLOAD 2

struct conntrack_value {
    __u64 seen_ns;
    __u64 flags;
    __u64 pending_bytes;
};

/* Traffic classes of allocation output, in UsageEgress field order. */
#define CLASS_PUBLIC_DIRECT 0
#define CLASS_PUBLIC_INGRESS 1
#define CLASS_PRIVATE 2
#define CLASS_PLATFORM 3
#define CLASS_PROBE 4

struct usage_counters {
    __u64 bytes[5];
};

/* traffic_class_trie values. */
#define TRAFFIC_INGRESS 1
#define TRAFFIC_PLATFORM 2

struct packet_info {
    __u8 src_ip[16];
    __u8 dst_ip[16];
    __u32 l4_off;
    __u8 protocol;
    __u8 family;
};

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 10000);
    __type(key, struct connection_key);
    __type(value, struct conntrack_value);
} conntrack_inner_template SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_HASH);
    __uint(max_entries, 2048);
    __type(key, __u64);
    __type(value, struct usage_counters);
} usage_counters SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, __u64);
} usage_unattributed SEC(".maps");

/* Packets addressed to a local identity with no live veth (a sleeping
 * serverless allocation), keyed by normalized destination IP. Userspace
 * polls and diffs these to wake the allocation's service; the packets
 * themselves are dropped. Entries are pre-created by userspace when it
 * seeds a local identity, mirroring usage counters, but the program also
 * creates on first sight so a seed/ordering gap cannot lose the wake. */
struct wake_ip_key {
    __u8 ip[16];
};

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_HASH);
    __uint(max_entries, 4096);
    __type(key, struct wake_ip_key);
    __type(value, __u64);
} wake_counters SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LPM_TRIE);
    __uint(max_entries, 1024);
    __type(key, struct identity_key);
    __type(value, __u8);
    __uint(map_flags, BPF_F_NO_PREALLOC);
} traffic_class_trie SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH_OF_MAPS);
    __uint(max_entries, 1024);
    __type(key, __u32);
    __array(values, typeof(conntrack_inner_template));
} conntrack_matrix SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LPM_TRIE);
    __uint(max_entries, 65536);
    __type(key, struct identity_key);
    __type(value, struct identity_value);
    __uint(map_flags, BPF_F_NO_PREALLOC);
} cluster_identity_trie SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 1024);
    __type(key, __u32);
    __type(value, struct container_policy);
} container_policy_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 4096);
    __type(key, __u32);
    __type(value, __u8);
} interface_role_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct host_ip_value);
} local_node_map SEC(".maps");

static __always_inline int ipv6_equal(const __u8 a[16], const __u8 b[16])
{
    int i;
#pragma unroll
    for (i = 0; i < 16; i++) {
        if (a[i] != b[i]) {
            return 0;
        }
    }
    return 1;
}

static __always_inline int ipv6_is_unspecified(const __u8 ip[16])
{
    int i;
#pragma unroll
    for (i = 0; i < 16; i++) {
        if (ip[i] != 0) {
            return 0;
        }
    }
    return 1;
}

static __always_inline int ipv6_is_multicast(const __u8 ip[16])
{
    return ip[0] == 0xff;
}

static __always_inline void normalize_ipv4(const __u8 ip[4], __u8 out[16])
{
    __builtin_memset(out, 0, 16);
    out[10] = 0xff;
    out[11] = 0xff;
    __builtin_memcpy(&out[12], ip, 4);
}

static __always_inline int load_packet(struct __sk_buff *skb, struct packet_info *packet, int has_ethernet)
{
    __u8 version;
    __u32 l3_off;
    if (has_ethernet) {
        struct ethhdr eth;
        if (bpf_skb_load_bytes(skb, 0, &eth, sizeof(eth)) < 0) {
            return -1;
        }
        if (eth.h_proto == bpf_htons(ETH_P_IP)) {
            version = 4;
        } else if (eth.h_proto == bpf_htons(ETH_P_IPV6)) {
            version = 6;
        } else {
            return -2;
        }
        l3_off = sizeof(struct ethhdr);
    } else {
        __u8 first_byte;
        if (bpf_skb_load_bytes(skb, 0, &first_byte, sizeof(first_byte)) < 0) {
            return -1;
        }
        version = first_byte >> 4;
        if (version != 4 && version != 6) {
            return -2;
        }
        l3_off = 0;
    }

    if (version == 4) {
        struct iphdr ip4;
        if (bpf_skb_load_bytes(skb, l3_off, &ip4, sizeof(ip4)) < 0) {
            return -1;
        }
        if (ip4.version != 4 || ip4.ihl < 5) {
            return -1;
        }
        normalize_ipv4((const __u8 *)&ip4.saddr, packet->src_ip);
        normalize_ipv4((const __u8 *)&ip4.daddr, packet->dst_ip);
        packet->l4_off = l3_off + ((__u32)ip4.ihl * 4);
        packet->protocol = ip4.protocol;
        packet->family = 4;
        return 0;
    }

    struct ipv6hdr ip6;
    if (bpf_skb_load_bytes(skb, l3_off, &ip6, sizeof(ip6)) < 0) {
        return -1;
    }
    if (ip6.version != 6) {
        return -1;
    }
    __builtin_memcpy(packet->src_ip, &ip6.saddr, 16);
    __builtin_memcpy(packet->dst_ip, &ip6.daddr, 16);
    packet->l4_off = l3_off + sizeof(ip6);
    packet->protocol = ip6.nexthdr;
    packet->family = 6;
    return 0;
}

static __always_inline int load_ports(struct __sk_buff *skb, const struct packet_info *packet,
                                      __be16 *sport, __be16 *dport)
{
    if (packet->protocol == IPPROTO_TCP) {
        struct tcphdr tcp;
        if (bpf_skb_load_bytes(skb, packet->l4_off, &tcp, sizeof(tcp)) < 0) {
            return -1;
        }
        *sport = tcp.source;
        *dport = tcp.dest;
        return 0;
    }

    if (packet->protocol == IPPROTO_UDP) {
        struct udphdr udp;
        if (bpf_skb_load_bytes(skb, packet->l4_off, &udp, sizeof(udp)) < 0) {
            return -1;
        }
        *sport = udp.source;
        *dport = udp.dest;
        return 0;
    }

    return -1;
}

static __always_inline int packet_matches_policy(const __u8 ip[16], const struct packet_info *packet,
                                                  const struct container_policy *policy)
{
    if (packet->family == 4) {
        return __builtin_memcmp(&ip[12], policy->ipv4, 4) == 0;
    }
    return ipv6_equal(ip, policy->ipv6);
}

struct arp_ipv4 {
    __be16 htype;
    __be16 ptype;
    __u8 hlen;
    __u8 plen;
    __be16 operation;
    __u8 sender_hardware[6];
    __u8 sender_ip[4];
    __u8 target_hardware[6];
    __u8 target_ip[4];
};

static __always_inline int validate_arp(struct __sk_buff *skb, const struct container_policy *policy,
                                        int from_container)
{
    struct ethhdr eth;
    struct arp_ipv4 arp;
    if (bpf_skb_load_bytes(skb, 0, &eth, sizeof(eth)) < 0 ||
        eth.h_proto != bpf_htons(ETH_P_ARP) ||
        bpf_skb_load_bytes(skb, sizeof(eth), &arp, sizeof(arp)) < 0) {
        return 0;
    }
    if (arp.htype != bpf_htons(1) || arp.ptype != bpf_htons(ETH_P_IP) ||
        arp.hlen != 6 || arp.plen != 4) {
        return 0;
    }
    const __u8 *workload_ip = from_container ? arp.sender_ip : arp.target_ip;
    return __builtin_memcmp(workload_ip, policy->ipv4, 4) == 0;
}

#ifndef IPPROTO_ICMPV6
#define IPPROTO_ICMPV6 58
#endif

#define ICMPV6_ROUTER_ADVERT 134
#define ICMPV6_NEIGHBOR_SOLICIT 135
#define ICMPV6_NEIGHBOR_ADVERT 136

struct icmpv6_nd {
    __u8 type;
    __u8 code;
    __be16 checksum;
    __be32 reserved;
    __u8 target[16];
};

/*
 * IPv6 neighbour discovery is the moral equivalent of ARP: a neighbour
 * solicitation is addressed to the solicited-node multicast group, not to the
 * workload address, so the ordinary destination check drops it and the two
 * workloads can never resolve each other's MAC. Scope it to the workload's own
 * address exactly the way validate_arp() scopes ARP, so a workload can only be
 * solicited for, or advertise, the address the policy assigned it. Identity
 * checks on the actual IP flows are untouched.
 */
static __always_inline int validate_ndp(struct __sk_buff *skb, const struct packet_info *packet,
                                        const struct container_policy *policy, int from_container)
{
    if (packet->family != 6 || packet->protocol != IPPROTO_ICMPV6) {
        return 0;
    }
    struct icmpv6_nd nd;
    if (bpf_skb_load_bytes(skb, packet->l4_off, &nd, sizeof(nd)) < 0) {
        return 0;
    }
    if (from_container) {
        if (nd.type == ICMPV6_NEIGHBOR_SOLICIT) {
            return ipv6_equal(packet->src_ip, policy->ipv6) || ipv6_is_unspecified(packet->src_ip);
        }
        if (nd.type == ICMPV6_NEIGHBOR_ADVERT) {
            return ipv6_equal(packet->src_ip, policy->ipv6) && ipv6_equal(nd.target, policy->ipv6);
        }
        return 0;
    }
    if (nd.type == ICMPV6_NEIGHBOR_SOLICIT) {
        return ipv6_equal(nd.target, policy->ipv6);
    }
    if (nd.type == ICMPV6_NEIGHBOR_ADVERT) {
        return ipv6_equal(packet->dst_ip, policy->ipv6);
    }
    if (nd.type == ICMPV6_ROUTER_ADVERT) {
        return ipv6_equal(packet->dst_ip, policy->ipv6) || ipv6_is_multicast(packet->dst_ip);
    }
    return 0;
}

static __always_inline struct identity_value *lookup_identity(const __u8 ip[16])
{
    struct identity_key key = {
        .prefixlen = 128,
    };
    __builtin_memcpy(key.ip_address, ip, sizeof(key.ip_address));
    return bpf_map_lookup_elem(&cluster_identity_trie, &key);
}

static __always_inline int conntrack_contains(__u32 ifindex, struct connection_key *key)
{
    void *inner = bpf_map_lookup_elem(&conntrack_matrix, &ifindex);
    if (!inner) {
        return 0;
    }
    return bpf_map_lookup_elem(inner, key) != 0;
}

static __always_inline int conntrack_record(__u32 ifindex, struct connection_key *key)
{
    void *inner = bpf_map_lookup_elem(&conntrack_matrix, &ifindex);
    if (!inner) {
        return -1;
    }
    struct conntrack_value *flow = bpf_map_lookup_elem(inner, key);
    if (flow) {
        flow->seen_ns = bpf_ktime_get_ns();
        return 0;
    }
    struct conntrack_value fresh = {.seen_ns = bpf_ktime_get_ns()};
    if (bpf_map_update_elem(inner, key, &fresh, BPF_NOEXIST) < 0) {
        return -1;
    }
    return 0;
}

static __always_inline __u8 traffic_class(const __u8 ip[16])
{
    struct identity_key key = {.prefixlen = 128};
    __builtin_memcpy(key.ip_address, ip, sizeof(key.ip_address));
    __u8 *value = bpf_map_lookup_elem(&traffic_class_trie, &key);
    return value ? *value : 0;
}

static __always_inline void count_wake(const __u8 dst_ip[16])
{
    struct wake_ip_key key = {};
    __builtin_memcpy(key.ip, dst_ip, sizeof(key.ip));
    __u64 *packets = bpf_map_lookup_elem(&wake_counters, &key);
    if (!packets) {
        __u64 one = 1;
        bpf_map_update_elem(&wake_counters, &key, &one, BPF_NOEXIST);
        return;
    }
    *packets += 1;
}

static __always_inline void count_egress(__u64 token, __u32 class, __u64 bytes)
{
    if (bytes == 0 || class > CLASS_PROBE) {
        return;
    }
    struct usage_counters *counters = bpf_map_lookup_elem(&usage_counters, &token);
    if (!counters) {
        __u32 zero = 0;
        __u64 *lost = bpf_map_lookup_elem(&usage_unattributed, &zero);
        if (lost) {
            *lost += bytes;
        }
        return;
    }
    counters->bytes[class & 7] += bytes;
}

/* Logical IP bytes: the frame minus Ethernet, with GSO superframes
 * normalized by adding back per-segment L3+L4 headers. Retransmissions seen
 * at this interface count; wire overhead beyond IP does not. */
static __always_inline __u64 logical_bytes(struct __sk_buff *skb, const struct packet_info *packet)
{
    __u64 bytes = skb->len > ETH_HLEN ? skb->len - ETH_HLEN : 0;
    __u32 segs = skb->gso_segs;
    if (segs > 1) {
        __u32 l4 = 0;
        if (packet->protocol == IPPROTO_TCP) {
            struct tcphdr tcp;
            if (bpf_skb_load_bytes(skb, packet->l4_off, &tcp, sizeof(tcp)) == 0) {
                l4 = (__u32)tcp.doff * 4;
            }
        } else if (packet->protocol == IPPROTO_UDP) {
            l4 = sizeof(struct udphdr);
        }
        __u32 headers = (packet->l4_off > ETH_HLEN ? packet->l4_off - ETH_HLEN : 0) + l4;
        bytes += (__u64)(segs - 1) * headers;
    }
    return bytes;
}

static __always_inline __u32 tcp_payload_len(struct __sk_buff *skb, const struct packet_info *packet,
                                             __u8 *flags_out)
{
    *flags_out = 0;
    if (packet->protocol != IPPROTO_TCP) {
        return 0;
    }
    struct tcphdr tcp;
    if (bpf_skb_load_bytes(skb, packet->l4_off, &tcp, sizeof(tcp)) < 0) {
        return 0;
    }
    *flags_out = ((__u8 *)&tcp)[13];
    __u32 end = packet->l4_off + (__u32)tcp.doff * 4;
    return skb->len > end ? skb->len - end : 0;
}

/* Count one allowed packet leaving the allocation, holding control-only
 * ingress-flow bytes as pending until payload or connection end. */
static __always_inline void count_output(struct __sk_buff *skb, __u32 ifindex,
                                         const struct container_policy *policy,
                                         const struct packet_info *packet, __u32 class)
{
    __u64 bytes = logical_bytes(skb, packet);
    if (class != CLASS_PUBLIC_INGRESS || packet->protocol != IPPROTO_TCP) {
        count_egress(policy->usage_token, class, bytes);
        return;
    }
    __be16 sport, dport;
    if (load_ports(skb, packet, &sport, &dport) < 0) {
        count_egress(policy->usage_token, class, bytes);
        return;
    }
    struct connection_key key = {.src_port = sport, .dst_port = dport, .protocol = packet->protocol};
    __builtin_memcpy(key.src_ip, packet->src_ip, sizeof(key.src_ip));
    __builtin_memcpy(key.dst_ip, packet->dst_ip, sizeof(key.dst_ip));
    void *inner = bpf_map_lookup_elem(&conntrack_matrix, &ifindex);
    struct conntrack_value *flow = inner ? bpf_map_lookup_elem(inner, &key) : 0;
    if (!flow || !(flow->flags & FLOW_INGRESS)) {
        count_egress(policy->usage_token, class, bytes);
        return;
    }
    __u8 tcp_flags;
    __u32 payload = tcp_payload_len(skb, packet, &tcp_flags);
    if ((flow->flags & FLOW_PAYLOAD) || payload > 0) {
        __u64 pending = flow->pending_bytes;
        flow->flags |= FLOW_PAYLOAD;
        flow->pending_bytes = 0;
        count_egress(policy->usage_token, CLASS_PUBLIC_INGRESS, pending + bytes);
        return;
    }
    if (tcp_flags & 0x05) { /* FIN or RST before any payload: a probe. */
        __u64 pending = flow->pending_bytes;
        flow->pending_bytes = 0;
        count_egress(policy->usage_token, CLASS_PROBE, pending + bytes);
        return;
    }
    flow->pending_bytes += bytes;
}

static __always_inline int handle_container_ingress(struct __sk_buff *skb)
{
    __u32 ifindex = skb->ifindex;
    struct container_policy *policy = bpf_map_lookup_elem(&container_policy_map, &ifindex);
    if (!policy) {
        return TC_ACT_SHOT;
    }

    struct packet_info packet = {};
    int rc = load_packet(skb, &packet, 1);
    if (rc == -2) {
        return validate_arp(skb, policy, 1) ? TC_ACT_OK : TC_ACT_SHOT;
    }
    if (rc < 0) {
        return TC_ACT_SHOT;
    }

    if (validate_ndp(skb, &packet, policy, 1)) {
        return TC_ACT_OK;
    }

    if (!packet_matches_policy(packet.src_ip, &packet, policy)) {
        return TC_ACT_SHOT;
    }

    struct identity_value *dst_identity = lookup_identity(packet.dst_ip);
    if (dst_identity) {
        if (dst_identity->network_identity != policy->network_identity) {
            return TC_ACT_SHOT;
        }

        __u32 zero = 0;
        struct host_ip_value *local_host = bpf_map_lookup_elem(&local_node_map, &zero);
        if (!local_host) {
            return TC_ACT_SHOT;
        }

        if (ipv6_equal(dst_identity->host_ip, local_host->ip) && dst_identity->veth_ifindex != 0) {
            (void)bpf_skb_change_type(skb, PACKET_HOST);
            int redirected = bpf_redirect_peer(dst_identity->veth_ifindex, 0);
            if (redirected == TC_ACT_REDIRECT) {
                count_output(skb, ifindex, policy, &packet, CLASS_PRIVATE);
            }
            return redirected;
        }

        if (ipv6_equal(dst_identity->host_ip, local_host->ip)) {
            /* Local identity with no live veth: a sleeping allocation (or
             * one still starting). Count the wake attempt and drop the
             * packet instead of metering it as delivered private output;
             * TCP retransmission covers the wake gap. */
            count_wake(packet.dst_ip);
            return TC_ACT_SHOT;
        }

        count_output(skb, ifindex, policy, &packet, CLASS_PRIVATE);
        return TC_ACT_OK;
    }

    /* Classify by trusted destination class before the generic path. */
    __u8 dst_class = traffic_class(packet.dst_ip);
    __u32 output_class = CLASS_PUBLIC_DIRECT;
    if (dst_class == TRAFFIC_PLATFORM) {
        output_class = CLASS_PLATFORM;
    } else if (dst_class == TRAFFIC_INGRESS) {
        output_class = CLASS_PUBLIC_INGRESS;
    }

    __be16 sport;
    __be16 dport;
    if (load_ports(skb, &packet, &sport, &dport) < 0) {
        /* Allowed ICMP and other port-less output. */
        count_output(skb, ifindex, policy, &packet, output_class);
        return TC_ACT_OK;
    }

    struct connection_key key = {
        .src_port = sport,
        .dst_port = dport,
        .protocol = packet.protocol,
    };
    __builtin_memcpy(key.src_ip, packet.src_ip, sizeof(key.src_ip));
    __builtin_memcpy(key.dst_ip, packet.dst_ip, sizeof(key.dst_ip));

    if (conntrack_record(ifindex, &key) < 0) {
        return TC_ACT_SHOT;
    }
    count_output(skb, ifindex, policy, &packet, output_class);

    return TC_ACT_OK;
}

static __always_inline int handle_wireguard_ingress(struct __sk_buff *skb)
{
    struct packet_info packet = {};
    int rc = load_packet(skb, &packet, 0);
    if (rc < 0) {
        return TC_ACT_SHOT;
    }

    struct identity_value *src_identity = lookup_identity(packet.src_ip);
    struct identity_value *dst_identity = lookup_identity(packet.dst_ip);
    if (!src_identity || !dst_identity) {
        return TC_ACT_SHOT;
    }
    if (src_identity->network_identity != dst_identity->network_identity) {
        return TC_ACT_SHOT;
    }

    __u32 zero = 0;
    struct host_ip_value *local_host = bpf_map_lookup_elem(&local_node_map, &zero);
    if (!local_host) {
        return TC_ACT_SHOT;
    }
    if (!ipv6_equal(dst_identity->host_ip, local_host->ip)) {
        return TC_ACT_SHOT;
    }
    if (dst_identity->veth_ifindex == 0) {
        /* Sleeping local allocation: count the wake attempt, then drop. */
        count_wake(packet.dst_ip);
        return TC_ACT_SHOT;
    }

    return TC_ACT_OK;
}

static __always_inline int handle_container_egress(struct __sk_buff *skb)
{
    __u32 ifindex = skb->ifindex;
    struct container_policy *policy = bpf_map_lookup_elem(&container_policy_map, &ifindex);
    if (!policy) {
        return TC_ACT_SHOT;
    }

    struct packet_info packet = {};
    int rc = load_packet(skb, &packet, 1);
    if (rc == -2) {
        return validate_arp(skb, policy, 0) ? TC_ACT_OK : TC_ACT_SHOT;
    }
    if (rc < 0) {
        return TC_ACT_SHOT;
    }

    if (validate_ndp(skb, &packet, policy, 0)) {
        return TC_ACT_OK;
    }

    if (!packet_matches_policy(packet.dst_ip, &packet, policy)) {
        return TC_ACT_SHOT;
    }

    struct identity_value *src_identity = lookup_identity(packet.src_ip);
    struct identity_value *dst_identity = lookup_identity(packet.dst_ip);
    if (src_identity) {
        if (!dst_identity) {
            return TC_ACT_SHOT;
        }
        if (dst_identity->network_identity != policy->network_identity) {
            return TC_ACT_SHOT;
        }
        if (src_identity->network_identity != dst_identity->network_identity) {
            return TC_ACT_SHOT;
        }
        return TC_ACT_OK;
    }

    __be16 sport;
    __be16 dport;
    if (load_ports(skb, &packet, &sport, &dport) < 0) {
        return TC_ACT_SHOT;
    }

    struct connection_key reverse = {
        .src_port = dport,
        .dst_port = sport,
        .protocol = packet.protocol,
    };
    __builtin_memcpy(reverse.src_ip, packet.dst_ip, sizeof(reverse.src_ip));
    __builtin_memcpy(reverse.dst_ip, packet.src_ip, sizeof(reverse.dst_ip));

    if (conntrack_contains(ifindex, &reverse)) {
        return TC_ACT_OK;
    }

    /* Trusted ingress proxies may open TCP connections to allocations;
     * without this the reverse-conntrack check above drops Envoy traffic.
     * Record the flow so response bytes classify as public ingress, or as
     * a probe when the flow never carries payload. */
    if (packet.protocol == IPPROTO_TCP && traffic_class(packet.src_ip) == TRAFFIC_INGRESS) {
        void *inner = bpf_map_lookup_elem(&conntrack_matrix, &ifindex);
        if (!inner) {
            return TC_ACT_SHOT;
        }
        __u8 tcp_flags;
        __u32 payload = tcp_payload_len(skb, &packet, &tcp_flags);
        struct conntrack_value *flow = bpf_map_lookup_elem(inner, &reverse);
        if (flow) {
            flow->seen_ns = bpf_ktime_get_ns();
            if (payload > 0) {
                flow->flags |= FLOW_PAYLOAD;
            }
            return TC_ACT_OK;
        }
        struct conntrack_value fresh = {.seen_ns = bpf_ktime_get_ns(), .flags = FLOW_INGRESS};
        if (payload > 0) {
            fresh.flags |= FLOW_PAYLOAD;
        }
        if (bpf_map_update_elem(inner, &reverse, &fresh, BPF_NOEXIST) < 0) {
            return TC_ACT_SHOT;
        }
        return TC_ACT_OK;
    }

    return TC_ACT_SHOT;
}

SEC("tcx/ingress")
int tcx_ingress(struct __sk_buff *skb)
{
    __u32 ifindex = skb->ifindex;
    __u8 *role = bpf_map_lookup_elem(&interface_role_map, &ifindex);
    if (!role) {
        return TC_ACT_SHOT;
    }

    if (*role == ROLE_CONTAINER) {
        return handle_container_ingress(skb);
    }
    if (*role == ROLE_WIREGUARD) {
        return handle_wireguard_ingress(skb);
    }

    return TC_ACT_SHOT;
}

SEC("tcx/egress")
int tcx_egress(struct __sk_buff *skb)
{
    __u32 ifindex = skb->ifindex;
    __u8 *role = bpf_map_lookup_elem(&interface_role_map, &ifindex);
    if (!role) {
        return TC_ACT_OK;
    }

    if (*role == ROLE_CONTAINER) {
        return handle_container_egress(skb);
    }

    return TC_ACT_OK;
}

char __license[] SEC("license") = "GPL";
