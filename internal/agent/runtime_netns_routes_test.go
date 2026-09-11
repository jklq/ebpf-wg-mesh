package agent

import (
	"net"
	"strings"
	"testing"
)

func TestDecideWorkloadPoolRouteFailClosed(t *testing.T) {
	_, pool, err := net.ParseCIDR("10.200.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	if err := decideWorkloadPoolRoute(nil, nil, 0, 0); err != nil {
		t.Fatalf("empty family: %v", err)
	}
	if err := decideWorkloadPoolRoute(net.ParseIP("10.200.0.1"), nil, 2, 0); err == nil {
		t.Fatal("default without pool must fail")
	} else if !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("error = %v", err)
	}
	if err := decideWorkloadPoolRoute(nil, pool, 0, 2); err == nil {
		t.Fatal("pool without default must fail")
	}
	if err := decideWorkloadPoolRoute(net.ParseIP("192.0.2.1"), pool, 2, 3); err == nil {
		t.Fatal("different links must fail")
	} else if !strings.Contains(err.Error(), "different links") {
		t.Fatalf("error = %v", err)
	}
	if err := decideWorkloadPoolRoute(net.ParseIP("192.0.2.1"), pool, 2, 2); err != nil {
		t.Fatalf("valid rewrite: %v", err)
	}
}
