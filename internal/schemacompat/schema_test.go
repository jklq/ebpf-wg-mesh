package schemacompat

import "testing"

func TestSchemaCompatibilityBoundary(t *testing.T) {
	for _, tc := range []struct {
		name             string
		status           Status
		release, minimum int
		allowed          bool
	}{
		{"legacy", Status{43, 43, 1}, 43, 43, true},
		{"additive overlap", Status{44, 43, 1}, 43, 43, true},
		{"new binary before cleanup", Status{44, 43, 1}, 45, 44, true},
		{"cleanup excludes old binary", Status{45, 45, 1}, 44, 44, false},
		{"missing required structures", Status{43, 43, 1}, 44, 44, false},
		{"invalid boundary", Status{44, 45, 1}, 45, 43, false},
		{"missing version", Status{}, 43, 43, false},
		{"multiple versions", Status{44, 43, 2}, 43, 43, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.status.Supports(tc.release, tc.minimum); got != tc.allowed {
				t.Fatalf("Supports = %v", got)
			}
		})
	}
}
