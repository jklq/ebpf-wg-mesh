package firewall

import (
	"bytes"
	"testing"
	"time"
)

func seedSyncPacket() []byte {
	codec, err := newSyncCodec("seed-node", []byte("0123456789abcdef0123456789abcdef"), time.Minute)
	if err != nil {
		return nil
	}
	codec.nowFn = func() time.Time { return time.Unix(1_700_000_000, 0) }
	pkt, err := codec.encodeConnEvent([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15})
	if err != nil {
		return nil
	}
	return pkt
}

func FuzzParseSyncKey_NoPanic(f *testing.F) {
	f.Add("")
	f.Add("%%%")
	f.Add("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	f.Add("3031323334353637383961626364656630313233343536373839616263646566")

	f.Fuzz(func(t *testing.T, raw string) {
		_, _ = parseSyncKey(raw)
	})
}

func FuzzDecodeConnEvent_NoPanic(f *testing.F) {
	f.Add([]byte{})
	if pkt := seedSyncPacket(); pkt != nil {
		f.Add(pkt)
	}

	f.Fuzz(func(t *testing.T, pkt []byte) {
		c, err := newSyncCodec("receiver", []byte("0123456789abcdef0123456789abcdef"), 2*time.Minute)
		if err != nil {
			t.Fatalf("newSyncCodec: %v", err)
		}
		c.nowFn = func() time.Time { return time.Unix(1_700_000_000, 0) }

		key, sender, err := c.decodeConnEvent(pkt)
		if err == nil {
			if len(key) != syncConnKeyLen {
				t.Fatalf("decoded key len mismatch: %d", len(key))
			}
			if sender == "" || len(sender) > syncMaxSenderLen {
				t.Fatalf("decoded sender invalid: %q", sender)
			}
		}
	})
}

func FuzzSyncCodecRoundTrip(f *testing.F) {
	f.Add("node-a", []byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}, []byte("0123456789abcdef0123456789abcdef"))
	f.Add("receiver", []byte{9, 8, 7, 6, 5}, []byte("tiny-key"))

	f.Fuzz(func(t *testing.T, sender string, connKey []byte, sharedKey []byte) {
		if sender == "" {
			sender = "fuzz-node"
		}
		if len(sender) > syncMaxSenderLen {
			sender = sender[:syncMaxSenderLen]
		}

		if len(sharedKey) < 16 {
			padded := make([]byte, 16)
			copy(padded, sharedKey)
			for i := len(sharedKey); i < len(padded); i++ {
				padded[i] = byte(i)
			}
			sharedKey = padded
		}
		if len(connKey) < syncConnKeyLen {
			padded := make([]byte, syncConnKeyLen)
			copy(padded, connKey)
			for i := len(connKey); i < len(padded); i++ {
				padded[i] = byte(i)
			}
			connKey = padded
		}

		now := time.Unix(1_700_000_000, 0)
		enc := mustNewCodec(t, sender, sharedKey, 2*time.Minute)
		dec := mustNewCodec(t, "fuzz-receiver", sharedKey, 2*time.Minute)
		enc.nowFn = func() time.Time { return now }
		dec.nowFn = func() time.Time { return now }

		pkt, err := enc.encodeConnEvent(connKey)
		if err != nil {
			t.Fatalf("encodeConnEvent: %v", err)
		}
		gotKey, gotSender, err := dec.decodeConnEvent(pkt)
		if err != nil {
			t.Fatalf("decodeConnEvent: %v", err)
		}
		if gotSender != sender {
			t.Fatalf("sender mismatch: got=%q want=%q", gotSender, sender)
		}
		if !bytes.Equal(gotKey, connKey[:syncConnKeyLen]) {
			t.Fatalf("decoded conn key mismatch")
		}
	})
}
