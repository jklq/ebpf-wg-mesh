package main

import "testing"

func TestValidateAgentBindings(t *testing.T) {
	hosts := map[string]hostInfo{
		"controlplane": {Name: "controlplane"},
		"agent-a":      {Name: "agent-a"},
		"agent-b":      {Name: "agent-b"},
	}
	cases := []struct {
		name    string
		tokens  map[string]string
		wantErr bool
	}{
		{"matching hosts and tokens", map[string]string{"agent-a": "token-a", "agent-b": "token-b"}, false},
		{"host without token", map[string]string{"agent-a": "token-a"}, true},
		{"token without host", map[string]string{"agent-a": "token-a", "agent-b": "token-b", "agent-c": "token-c"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgentBindings(hosts, tc.tokens)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateAgentBindings() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
