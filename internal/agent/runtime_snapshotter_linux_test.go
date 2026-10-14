//go:build linux

package agent

import (
	"testing"

	introspection "github.com/containerd/containerd/api/services/introspection/v1"
	"google.golang.org/genproto/googleapis/rpc/status"
)

func TestPreferredSnapshotter(t *testing.T) {
	ready := func(name string) *introspection.Plugin {
		return &introspection.Plugin{ID: name, Type: "io.containerd.snapshotter.v1"}
	}
	unsupported := ready("overlayfs")
	unsupported.InitErr = &status.Status{Code: 2, Message: "unsupported filesystem"}
	for _, tt := range []struct {
		name    string
		plugins []*introspection.Plugin
		want    string
	}{
		{"overlay available", []*introspection.Plugin{ready("native"), ready("overlayfs")}, "overlayfs"},
		{"overlay unsupported", []*introspection.Plugin{unsupported, ready("native")}, "native"},
		{"overlay absent", []*introspection.Plugin{ready("native")}, "native"},
		{"no usable snapshotter", []*introspection.Plugin{unsupported}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := preferredSnapshotter(tt.plugins)
			if got != tt.want || (err != nil) != (tt.want == "") {
				t.Fatalf("snapshotter = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}
