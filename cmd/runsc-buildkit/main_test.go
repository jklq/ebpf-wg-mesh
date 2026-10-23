package main

import (
	"slices"
	"testing"
)

func TestTranslateBuildKitInvocations(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "run",
			in:   []string{"--log", "/bk/runc-log.json", "--log-format", "json", "run", "--bundle", "/bk/bundle", "--keep", "abc123"},
			want: []string{"run", "--bundle", "/bk/bundle", "abc123"},
		},
		{
			name: "delete",
			in:   []string{"--log", "/bk/runc-log.json", "--log-format", "json", "delete", "abc123"},
			want: []string{"delete", "abc123"},
		},
		{
			name: "equals spelling and passthrough flags",
			in:   []string{"--log=/bk/log", "--root=/runc/root", "kill", "abc123", "SIGKILL"},
			want: []string{"kill", "abc123", "SIGKILL"},
		},
		{
			name: "unknown args forwarded",
			in:   []string{"state", "abc123"},
			want: []string{"state", "abc123"},
		},
	}
	for _, tc := range cases {
		if got := translate(tc.in); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: translate(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}
