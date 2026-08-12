package wgmesh

import (
	"testing"

	"ebof-wg-mesh/internal/config"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestDeviceUpdateDiffsPeersWithoutReplacingAllPeers(t *testing.T) {
	t.Parallel()

	privateKey := testKey(1)
	removedKey := testKey(2)
	changedKey := testKey(3)
	addedKey := testKey(4)
	current := mustWireGuardState(t, config.WireGuard{
		PrivateKey: privateKey.String(),
		ListenPort: 51820,
		Peers: []config.PeerConfig{
			testPeer("removed", removedKey, "[2001:db8::2]:51820", "fd00:44:2::/80", 5),
			testPeer("changed", changedKey, "[2001:db8::3]:51820", "fd00:44:3::/80", 5),
		},
	})
	next := mustWireGuardState(t, config.WireGuard{
		PrivateKey: privateKey.String(),
		ListenPort: 51820,
		Peers: []config.PeerConfig{
			testPeer("changed", changedKey, "[2001:db8::33]:51820", "fd00:44:33::/80", 0),
			testPeer("added", addedKey, "[2001:db8::4]:51820", "fd00:44:4::/80", 5),
		},
	})

	update := deviceUpdate(current, next)
	if update.ReplacePeers {
		t.Fatal("incremental update must not replace every peer")
	}
	if update.PrivateKey != nil || update.ListenPort != nil {
		t.Fatalf("unchanged device settings included in update: %+v", update)
	}
	byKey := make(map[wgtypes.Key]wgtypes.PeerConfig, len(update.Peers))
	for _, peer := range update.Peers {
		byKey[peer.PublicKey] = peer
	}
	if len(byKey) != 3 {
		t.Fatalf("expected remove, change, and add operations, got %+v", update.Peers)
	}
	if !byKey[removedKey].Remove {
		t.Fatalf("removed peer was not deleted: %+v", byKey[removedKey])
	}
	if got := byKey[changedKey]; got.Remove || !got.ReplaceAllowedIPs || got.Endpoint.String() != "[2001:db8::33]:51820" {
		t.Fatalf("changed peer was not replaced in place: %+v", got)
	} else if got.PersistentKeepaliveInterval == nil || *got.PersistentKeepaliveInterval != 0 {
		t.Fatalf("changed peer did not explicitly clear keepalive: %+v", got)
	}
	if got := byKey[addedKey]; got.Remove || !got.ReplaceAllowedIPs {
		t.Fatalf("added peer configuration is incomplete: %+v", got)
	}
}

func TestDeviceUpdateOmitsUnchangedPeers(t *testing.T) {
	t.Parallel()

	cfg := config.WireGuard{
		PrivateKey: testKey(1).String(),
		ListenPort: 51820,
		Peers: []config.PeerConfig{
			testPeer("peer", testKey(2), "[2001:db8::2]:51820", "fd00:44:2::/80", 5),
		},
	}
	current := mustWireGuardState(t, cfg)
	next := mustWireGuardState(t, cfg)

	update := deviceUpdate(current, next)
	if update.PrivateKey != nil || update.ListenPort != nil || len(update.Peers) != 0 {
		t.Fatalf("unchanged state produced update: %+v", update)
	}
}

func TestParseWireGuardStateNormalizesRoutes(t *testing.T) {
	t.Parallel()

	state := mustWireGuardState(t, config.WireGuard{
		PrivateKey: testKey(1).String(),
		Peers: []config.PeerConfig{
			testPeer("peer", testKey(2), "[2001:db8::2]:51820", "fd00:44:2::1234/80", 5),
		},
	})
	if _, ok := state.routes["fd00:44:2::/80"]; !ok {
		t.Fatalf("expected normalized route, got %+v", state.routes)
	}
}

func mustWireGuardState(t *testing.T, cfg config.WireGuard) wireGuardState {
	t.Helper()
	state, err := parseWireGuardState(cfg)
	if err != nil {
		t.Fatalf("parseWireGuardState: %v", err)
	}
	return state
}

func testPeer(name string, key wgtypes.Key, endpoint, allowedIP string, keepalive int) config.PeerConfig {
	return config.PeerConfig{
		Name:                 name,
		PublicKey:            key.String(),
		Endpoint:             endpoint,
		AllowedIPs:           []string{allowedIP},
		PersistentKeepaliveS: keepalive,
	}
}

func testKey(last byte) wgtypes.Key {
	var key wgtypes.Key
	key[len(key)-1] = last
	return key
}
