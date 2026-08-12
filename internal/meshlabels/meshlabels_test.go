package meshlabels

import (
	"net/netip"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()

	keys := NewKeys("", "")
	want := Identity{NetworkIdentity: 42, IPv6: netip.MustParseAddr("fd00:44::5")}

	labels := keys.Encode(want)
	if labels[DefaultEnvironmentKey] != "42" {
		t.Fatalf("unexpected environment label %q", labels[DefaultEnvironmentKey])
	}
	if labels[DefaultIPv6Key] != "fd00:44::5" {
		t.Fatalf("unexpected ipv6 label %q", labels[DefaultIPv6Key])
	}

	got, err := keys.Decode(labels)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got != want {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, want)
	}
}

func TestEncodeDecodeHonorsCustomKeys(t *testing.T) {
	t.Parallel()

	keys := NewKeys("custom.project", "custom.ipv6")
	want := Identity{NetworkIdentity: 7, IPv6: netip.MustParseAddr("fd00:44::1")}

	labels := keys.Encode(want)
	if _, ok := labels[DefaultEnvironmentKey]; ok {
		t.Fatal("custom keys must not write the default project key")
	}
	got, err := keys.Decode(labels)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got != want {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, want)
	}

	if _, err := NewKeys("", "").Decode(labels); err == nil {
		t.Fatal("expected default keys to reject labels written under custom keys")
	}
}

func TestDecodeRejectsBadLabels(t *testing.T) {
	t.Parallel()

	keys := NewKeys("", "")
	cases := map[string]map[string]string{
		"nil labels":       nil,
		"empty labels":     {},
		"missing project":  {DefaultIPv6Key: "fd00:44::5"},
		"missing ipv6":     {DefaultEnvironmentKey: "42"},
		"zero project":     {DefaultEnvironmentKey: "0", DefaultIPv6Key: "fd00:44::5"},
		"unparsed project": {DefaultEnvironmentKey: "abc", DefaultIPv6Key: "fd00:44::5"},
		"ipv4 address":     {DefaultEnvironmentKey: "42", DefaultIPv6Key: "10.0.0.1"},
		"unparsed ipv6":    {DefaultEnvironmentKey: "42", DefaultIPv6Key: "not-an-ip"},
	}
	for name, labels := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := keys.Decode(labels); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestEncodeOmitsUnsetIPv6(t *testing.T) {
	t.Parallel()

	keys := NewKeys("", "")
	labels := keys.Encode(Identity{NetworkIdentity: 42})
	if labels[DefaultIPv6Key] != "" {
		t.Fatalf("expected empty ipv6 label, got %q", labels[DefaultIPv6Key])
	}
	if _, err := keys.Decode(labels); err == nil {
		t.Fatal("expected decode to reject an identity without an address")
	}
}

func TestNetworkIdentityIsLenient(t *testing.T) {
	t.Parallel()

	keys := NewKeys("", "")
	if got := keys.NetworkIdentity(map[string]string{DefaultEnvironmentKey: "42"}); got != 42 {
		t.Fatalf("unexpected project id %d", got)
	}
	if got := keys.NetworkIdentity(map[string]string{DefaultEnvironmentKey: "nope"}); got != 0 {
		t.Fatalf("expected zero for malformed label, got %d", got)
	}
	if got := keys.NetworkIdentity(nil); got != 0 {
		t.Fatalf("expected zero for absent label, got %d", got)
	}
}
