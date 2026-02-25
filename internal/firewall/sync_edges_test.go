package firewall

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func mustNewCodec(t *testing.T, sender string, key []byte, window time.Duration) *syncCodec {
	t.Helper()
	c, err := newSyncCodec(sender, key, window)
	if err != nil {
		t.Fatalf("newSyncCodec(%q): %v", sender, err)
	}
	return c
}

func TestNewSyncCodecValidation(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")

	cases := []struct {
		name   string
		sender string
		key    []byte
		window time.Duration
	}{
		{name: "empty sender", sender: "", key: key, window: time.Minute},
		{name: "sender too long", sender: strings.Repeat("a", syncMaxSenderLen+1), key: key, window: time.Minute},
		{name: "short key", sender: "node-a", key: []byte("too-short"), window: time.Minute},
		{name: "non-positive replay window", sender: "node-a", key: key, window: 0},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newSyncCodec(tc.sender, tc.key, tc.window); err == nil {
				t.Fatalf("expected newSyncCodec error")
			}
		})
	}
}

func TestNewSyncCodecCopiesSharedKey(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	c := mustNewCodec(t, "node-a", key, time.Minute)
	key[0] ^= 0xff
	if c.sharedKey[0] == key[0] {
		t.Fatalf("codec key should be copied, not aliased")
	}
}

func TestParseSyncKeyEdgeCases(t *testing.T) {
	raw := []byte("0123456789abcdef0123456789abcdef")

	b64 := "\n\t " + base64.StdEncoding.EncodeToString(raw) + " \r\n"
	got, err := parseSyncKey(b64)
	if err != nil {
		t.Fatalf("parse base64 with whitespace: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("base64 parse mismatch")
	}

	hexUpper := strings.ToUpper(hex.EncodeToString(raw))
	got, err = parseSyncKey(hexUpper)
	if err != nil {
		t.Fatalf("parse upper hex: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("hex parse mismatch")
	}

	if _, err := parseSyncKey(" \n\t "); err == nil {
		t.Fatalf("expected empty key error")
	}
	if _, err := parseSyncKey("%%%not-a-key%%%"); err == nil {
		t.Fatalf("expected invalid key error")
	}
}

func TestEncodeConnEventRequiresMinimumConnKeyLen(t *testing.T) {
	c := mustNewCodec(t, "node-a", []byte("0123456789abcdef0123456789abcdef"), time.Minute)
	if _, err := c.encodeConnEvent([]byte{1, 2, 3}); err == nil {
		t.Fatalf("expected short conn key error")
	}
}

func TestEncodeDecodeUsesOnlyFirstConnKeyBytes(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_700_000_000, 0)
	sender := mustNewCodec(t, "node-a", key, time.Minute)
	receiver := mustNewCodec(t, "node-b", key, time.Minute)
	sender.nowFn = func() time.Time { return now }
	receiver.nowFn = func() time.Time { return now }

	connKey := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 99, 98}
	pkt, err := sender.encodeConnEvent(connKey)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	got, gotSender, err := receiver.decodeConnEvent(pkt)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotSender != "node-a" {
		t.Fatalf("sender mismatch: %q", gotSender)
	}
	if !bytes.Equal(got, connKey[:syncConnKeyLen]) {
		t.Fatalf("conn key mismatch")
	}
}

func TestDecodeConnEventRejectsMalformedPackets(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_700_000_000, 0)
	sender := mustNewCodec(t, "node-a", key, time.Minute)
	receiver := mustNewCodec(t, "node-b", key, time.Minute)
	sender.nowFn = func() time.Time { return now }
	receiver.nowFn = func() time.Time { return now }

	validPkt, err := sender.encodeConnEvent([]byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	cases := []struct {
		name string
		pkt  []byte
	}{
		{name: "too short", pkt: []byte{1, 2, 3}},
		{
			name: "wrong magic",
			pkt: func() []byte {
				p := append([]byte(nil), validPkt...)
				p[0] = 'X'
				return p
			}(),
		},
		{
			name: "wrong version",
			pkt: func() []byte {
				p := append([]byte(nil), validPkt...)
				p[4] = syncVersionV2 + 1
				return p
			}(),
		},
		{
			name: "sender length zero",
			pkt: func() []byte {
				p := append([]byte(nil), validPkt...)
				p[5] = 0
				return p
			}(),
		},
		{
			name: "sender length too large",
			pkt: func() []byte {
				p := append([]byte(nil), validPkt...)
				p[5] = syncMaxSenderLen + 1
				return p
			}(),
		},
		{
			name: "truncated payload",
			pkt:  append([]byte(nil), validPkt[:len(validPkt)-1]...),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := receiver.decodeConnEvent(tc.pkt); !errors.Is(err, errInvalidPacket) {
				t.Fatalf("expected errInvalidPacket, got %v", err)
			}
		})
	}
}

func TestDecodeConnEventClockSkewHandling(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	base := time.Unix(1_700_000_000, 0)

	t.Run("accepts small future skew", func(t *testing.T) {
		sender := mustNewCodec(t, "node-a", key, 30*time.Second)
		receiver := mustNewCodec(t, "node-b", key, 30*time.Second)
		sender.nowFn = func() time.Time { return base }
		receiver.nowFn = func() time.Time { return base.Add(-10 * time.Second) }

		pkt, err := sender.encodeConnEvent([]byte{4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if _, _, err := receiver.decodeConnEvent(pkt); err != nil {
			t.Fatalf("decode with skew: %v", err)
		}
	})

	t.Run("rejects far future skew", func(t *testing.T) {
		sender := mustNewCodec(t, "node-a", key, 30*time.Second)
		receiver := mustNewCodec(t, "node-b", key, 30*time.Second)
		sender.nowFn = func() time.Time { return base }
		receiver.nowFn = func() time.Time { return base.Add(-2 * time.Minute) }

		pkt, err := sender.encodeConnEvent([]byte{5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if _, _, err := receiver.decodeConnEvent(pkt); !errors.Is(err, errExpiredPacket) {
			t.Fatalf("expected expired packet, got %v", err)
		}
	})
}

func TestDecodeConnEventReturnsConnKeyCopy(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_700_000_000, 0)
	sender := mustNewCodec(t, "node-a", key, time.Minute)
	receiver := mustNewCodec(t, "node-b", key, time.Minute)
	sender.nowFn = func() time.Time { return now }
	receiver.nowFn = func() time.Time { return now }

	pkt, err := sender.encodeConnEvent([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	got, _, err := receiver.decodeConnEvent(pkt)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	pkt[22] = 99
	if got[0] != 1 {
		t.Fatalf("decoded key should be copied from packet")
	}
}

func TestReplayGuardPerSenderCounterTracking(t *testing.T) {
	g := replayGuard{lastCounter: map[string]uint64{}}

	if err := g.accept("node-a", 10); err != nil {
		t.Fatalf("accept first node-a counter: %v", err)
	}
	if err := g.accept("node-a", 10); !errors.Is(err, errReplayDetected) {
		t.Fatalf("expected replay for repeated node-a counter, got %v", err)
	}
	if err := g.accept("node-a", 11); err != nil {
		t.Fatalf("accept incremented node-a counter: %v", err)
	}
	if err := g.accept("node-b", 1); err != nil {
		t.Fatalf("accept first node-b counter: %v", err)
	}
	if err := g.accept("node-b", 0); !errors.Is(err, errReplayDetected) {
		t.Fatalf("expected replay for decremented node-b counter, got %v", err)
	}
}
