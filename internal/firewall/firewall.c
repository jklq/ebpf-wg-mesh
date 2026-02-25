//go:build ignore

#include <linux/bpf.h>
#include <linux/in.h>
#include <linux/ip.h>
#include <linux/pkt_cls.h>
#include <linux/tcp.h>
#include <linux/udp.h>

#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>

struct connection_key {
    __be32 src_ip;
    __be32 dst_ip;
    __be16 src_port;
    __be16 dst_port;
    __u8 protocol;
    __u8 _pad1;
    __u16 _pad2;
};

struct lpm_key {
    __u32 prefixlen;
    __be32 addr;
};

struct conn_event {
    struct connection_key key;
};

#define IPV4_FLAG_MF 0x2000
#define IPV4_FRAG_MASK 0x1FFF

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 131072);
    __type(key, struct connection_key);
    __type(value, __u64);
} conntrack_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LPM_TRIE);
    __uint(max_entries, 8192);
    __uint(map_flags, BPF_F_NO_PREALLOC);
    __type(key, struct lpm_key);
    __type(value, __u8);
} mesh_trust_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 24);
} conn_events SEC(".maps");

static __always_inline int is_trusted_ip(__be32 addr)
{
    struct lpm_key key = {
        .prefixlen = 32,
        .addr = addr,
    };

    return bpf_map_lookup_elem(&mesh_trust_map, &key) != 0;
}

static __always_inline int load_ipv4(struct __sk_buff *skb, struct iphdr *ip)
{
    if (bpf_skb_load_bytes(skb, 0, ip, sizeof(*ip)) < 0) {
        return -1;
    }
    if (ip->version != 4) {
        return -2;
    }
    if (ip->ihl < 5) {
        return -1;
    }
    return 0;
}

static __always_inline int load_ports(struct __sk_buff *skb, const struct iphdr *ip,
                                      __be16 *sport, __be16 *dport)
{
    if (bpf_ntohs(ip->frag_off) & (IPV4_FLAG_MF | IPV4_FRAG_MASK)) {
        return -1;
    }

    __u32 l4_off = (__u32)ip->ihl * 4;
    if (ip->protocol == IPPROTO_TCP) {
        struct tcphdr tcp;
        if (bpf_skb_load_bytes(skb, l4_off, &tcp, sizeof(tcp)) < 0) {
            return -1;
        }
        *sport = tcp.source;
        *dport = tcp.dest;
        return 0;
    }

    if (ip->protocol == IPPROTO_UDP) {
        struct udphdr udp;
        if (bpf_skb_load_bytes(skb, l4_off, &udp, sizeof(udp)) < 0) {
            return -1;
        }
        *sport = udp.source;
        *dport = udp.dest;
        return 0;
    }

    return -1;
}

SEC("tcx/egress")
int tcx_egress(struct __sk_buff *skb)
{
    struct iphdr ip;
    int rc = load_ipv4(skb, &ip);
    if (rc == -2) {
        return TC_ACT_OK;
    }
    if (rc < 0) {
        return TC_ACT_OK;
    }

    if (is_trusted_ip(ip.daddr)) {
        return TC_ACT_OK;
    }

    __be16 sport;
    __be16 dport;
    if (load_ports(skb, &ip, &sport, &dport) < 0) {
        return TC_ACT_OK;
    }

    struct connection_key key = {
        .src_ip = ip.saddr,
        .dst_ip = ip.daddr,
        .src_port = sport,
        .dst_port = dport,
        .protocol = ip.protocol,
    };

    __u64 now = bpf_ktime_get_ns();
    if (bpf_map_update_elem(&conntrack_map, &key, &now, BPF_NOEXIST) == 0) {
        struct conn_event *evt = bpf_ringbuf_reserve(&conn_events, sizeof(*evt), 0);
        if (evt) {
            evt->key = key;
            bpf_ringbuf_submit(evt, 0);
        }
    }

    return TC_ACT_OK;
}

SEC("tcx/ingress")
int tcx_ingress(struct __sk_buff *skb)
{
    struct iphdr ip;
    int rc = load_ipv4(skb, &ip);
    if (rc == -2) {
        return TC_ACT_OK;
    }
    if (rc < 0) {
        return TC_ACT_SHOT;
    }

    if (is_trusted_ip(ip.saddr)) {
        return TC_ACT_OK;
    }

    __be16 sport;
    __be16 dport;
    if (load_ports(skb, &ip, &sport, &dport) < 0) {
        return TC_ACT_SHOT;
    }

    struct connection_key key = {
        .src_ip = ip.daddr,
        .dst_ip = ip.saddr,
        .src_port = dport,
        .dst_port = sport,
        .protocol = ip.protocol,
    };

    __u64 *existing = bpf_map_lookup_elem(&conntrack_map, &key);
    if (existing) {
        return TC_ACT_OK;
    }

    return TC_ACT_SHOT;
}

char __license[] SEC("license") = "GPL";
