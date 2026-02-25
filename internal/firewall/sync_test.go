package firewall

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

func TestParseSyncKey(t *testing.T) {
	raw := []byte("0123456789abcdef0123456789abcdef")
	base64Key := base64.StdEncoding.EncodeToString(raw)
	hexKey := hex.EncodeToString(raw)

	k1, err := parseSyncKey(base64Key)
	if err != nil {
		t.Fatalf("base64 parse failed: %v", err)
	}
	if string(k1) != string(raw) {
		t.Fatalf("base64 parse mismatch")
	}

	k2, err := parseSyncKey(hexKey)
	if err != nil {
		t.Fatalf("hex parse failed: %v", err)
	}
	if string(k2) != string(raw) {
		t.Fatalf("hex parse mismatch")
	}
}

func TestSyncCodecEncodeDecode(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_700_000_000, 0)

	sender, err := newSyncCodec("node-a", key, 2*time.Minute)
	if err != nil {
		t.Fatalf("new sender codec: %v", err)
	}
	receiver, err := newSyncCodec("node-b", key, 2*time.Minute)
	if err != nil {
		t.Fatalf("new receiver codec: %v", err)
	}
	sender.nowFn = func() time.Time { return now }
	receiver.nowFn = func() time.Time { return now }

	connKey := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	pkt, err := sender.encodeConnEvent(connKey)
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	gotKey, gotSender, err := receiver.decodeConnEvent(pkt)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if gotSender != "node-a" {
		t.Fatalf("sender mismatch: %s", gotSender)
	}
	if string(gotKey) != string(connKey) {
		t.Fatalf("conn key mismatch")
	}
}

func TestSyncCodecRejectTamperedPacket(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_700_000_000, 0)

	sender, _ := newSyncCodec("node-a", key, 2*time.Minute)
	receiver, _ := newSyncCodec("node-b", key, 2*time.Minute)
	sender.nowFn = func() time.Time { return now }
	receiver.nowFn = func() time.Time { return now }

	connKey := []byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}
	pkt, err := sender.encodeConnEvent(connKey)
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}
	pkt[10] ^= 0xFF

	_, _, err = receiver.decodeConnEvent(pkt)
	if !errors.Is(err, errInvalidMAC) {
		t.Fatalf("expected invalid mac, got %v", err)
	}
}

func TestSyncCodecRejectReplay(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_700_000_000, 0)

	sender, _ := newSyncCodec("node-a", key, 2*time.Minute)
	receiver, _ := newSyncCodec("node-b", key, 2*time.Minute)
	sender.nowFn = func() time.Time { return now }
	receiver.nowFn = func() time.Time { return now }

	connKey := []byte{2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2}
	pkt, err := sender.encodeConnEvent(connKey)
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	if _, _, err := receiver.decodeConnEvent(pkt); err != nil {
		t.Fatalf("first decode failed: %v", err)
	}
	if _, _, err := receiver.decodeConnEvent(pkt); !errors.Is(err, errReplayDetected) {
		t.Fatalf("expected replay error, got %v", err)
	}
}

func TestSyncCodecRejectWrongKey(t *testing.T) {
	goodKey := []byte("0123456789abcdef0123456789abcdef")
	badKey := []byte("abcdef0123456789abcdef0123456789")
	now := time.Unix(1_700_000_000, 0)

	sender, _ := newSyncCodec("node-a", goodKey, 2*time.Minute)
	receiver, _ := newSyncCodec("node-b", badKey, 2*time.Minute)
	sender.nowFn = func() time.Time { return now }
	receiver.nowFn = func() time.Time { return now }

	connKey := []byte{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9}
	pkt, err := sender.encodeConnEvent(connKey)
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	if _, _, err := receiver.decodeConnEvent(pkt); !errors.Is(err, errInvalidMAC) {
		t.Fatalf("expected invalid mac for wrong key, got %v", err)
	}
}

func TestSyncCodecRejectExpired(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	base := time.Unix(1_700_000_000, 0)

	sender, _ := newSyncCodec("node-a", key, 30*time.Second)
	receiver, _ := newSyncCodec("node-b", key, 30*time.Second)
	sender.nowFn = func() time.Time { return base }
	receiver.nowFn = func() time.Time { return base.Add(2 * time.Minute) }

	connKey := []byte{3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3}
	pkt, err := sender.encodeConnEvent(connKey)
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	_, _, err = receiver.decodeConnEvent(pkt)
	if !errors.Is(err, errExpiredPacket) {
		t.Fatalf("expected expired packet, got %v", err)
	}
}
