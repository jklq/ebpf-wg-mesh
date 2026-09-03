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
};

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
    __type(value, __u64);
} conntrack_inner_template SEC(".maps");

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

    __u64 now = bpf_ktime_get_ns();
    if (bpf_map_update_elem(inner, key, &now, BPF_ANY) < 0) {
        return -1;
    }

    return 0;
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
            return bpf_redirect_peer(dst_identity->veth_ifindex, 0);
        }

        return TC_ACT_OK;
    }

    __be16 sport;
    __be16 dport;
    if (load_ports(skb, &packet, &sport, &dport) < 0) {
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
    if (!ipv6_equal(dst_identity->host_ip, local_host->ip) || dst_identity->veth_ifindex == 0) {
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
