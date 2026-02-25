package firewall

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
)

const (
	syncMagicV2      = "CTK2"
	syncVersionV2    = byte(1)
	syncConnKeyLen   = 16
	syncHMACLen      = 32
	syncHeaderLenV2  = 4 + 1 + 1 + 8 + 8 + syncConnKeyLen
	syncMaxSenderLen = 64
)

var (
	errInvalidPacket  = errors.New("invalid sync packet")
	errInvalidMAC     = errors.New("invalid sync packet mac")
	errReplayDetected = errors.New("replay detected")
	errExpiredPacket  = errors.New("expired sync packet")
)

type SyncRuntime struct {
	cancel context.CancelFunc
	done   chan struct{}
	pc     net.PacketConn
}

type replayGuard struct {
	mu          sync.Mutex
	lastCounter map[string]uint64
}

type syncCodec struct {
	sender       string
	sharedKey    []byte
	replayWindow time.Duration
	nowFn        func() time.Time
	counter      atomic.Uint64
	replay       replayGuard
}

func startSync(nodeName, listen, authKey string, replayWindow time.Duration, peers []string, reader *ringbuf.Reader, conntrack *ebpf.Map) (*SyncRuntime, error) {
	key, err := parseSyncKey(authKey)
	if err != nil {
		return nil, err
	}
	codec, err := newSyncCodec(nodeName, key, replayWindow)
	if err != nil {
		return nil, err
	}

	pc, err := net.ListenPacket("udp", listen)
	if err != nil {
		return nil, fmt.Errorf("listen state sync socket %s: %w", listen, err)
	}

	peerAddrs := make([]net.Addr, 0, len(peers))
	for _, peer := range peers {
		addr, err := net.ResolveUDPAddr("udp", peer)
		if err != nil {
			_ = pc.Close()
			return nil, fmt.Errorf("resolve sync peer %s: %w", peer, err)
		}
		peerAddrs = append(peerAddrs, addr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		eventToPeersLoop(ctx, reader, pc, peerAddrs, codec)
	}()

	go func() {
		defer wg.Done()
		peerToMapLoop(ctx, pc, conntrack, codec)
	}()

	go func() {
		wg.Wait()
		close(done)
	}()

	return &SyncRuntime{cancel: cancel, done: done, pc: pc}, nil
}

func newSyncCodec(sender string, sharedKey []byte, replayWindow time.Duration) (*syncCodec, error) {
	if sender == "" {
		return nil, errors.New("sync sender is required")
	}
	if len(sender) > syncMaxSenderLen {
		return nil, fmt.Errorf("sync sender too long: %d > %d", len(sender), syncMaxSenderLen)
	}
	if len(sharedKey) < 16 {
		return nil, errors.New("sync auth key must decode to at least 16 bytes")
	}
	if replayWindow <= 0 {
		return nil, errors.New("sync replay window must be > 0")
	}

	key := make([]byte, len(sharedKey))
	copy(key, sharedKey)
	return &syncCodec{
		sender:       sender,
		sharedKey:    key,
		replayWindow: replayWindow,
		nowFn:        time.Now,
		replay: replayGuard{
			lastCounter: make(map[string]uint64),
		},
	}, nil
}

func parseSyncKey(raw string) ([]byte, error) {
	raw = stringTrim(raw)
	if raw == "" {
		return nil, errors.New("sync auth key is required")
	}
	if isHexString(raw) {
		if k, err := hex.DecodeString(raw); err == nil {
			return k, nil
		}
	}
	if k, err := base64.StdEncoding.DecodeString(raw); err == nil {
		return k, nil
	}
	return nil, errors.New("sync auth key must be base64 or hex")
}

func stringTrim(v string) string {
	start := 0
	for start < len(v) && (v[start] == ' ' || v[start] == '\t' || v[start] == '\n' || v[start] == '\r') {
		start++
	}
	end := len(v)
	for end > start && (v[end-1] == ' ' || v[end-1] == '\t' || v[end-1] == '\n' || v[end-1] == '\r') {
		end--
	}
	return v[start:end]
}

func isHexString(v string) bool {
	if len(v)%2 != 0 || len(v) == 0 {
		return false
	}
	for i := 0; i < len(v); i++ {
		ch := v[i]
		isDigit := ch >= '0' && ch <= '9'
		isLower := ch >= 'a' && ch <= 'f'
		isUpper := ch >= 'A' && ch <= 'F'
		if !isDigit && !isLower && !isUpper {
			return false
		}
	}
	return true
}

func (c *syncCodec) encodeConnEvent(connKey []byte) ([]byte, error) {
	if len(connKey) < syncConnKeyLen {
		return nil, fmt.Errorf("conn key too short: %d", len(connKey))
	}

	sender := []byte(c.sender)
	ts := c.nowFn().Unix()
	counter := c.counter.Add(1)

	bodyLen := syncHeaderLenV2 + len(sender)
	msg := make([]byte, bodyLen+syncHMACLen)
	copy(msg[:4], []byte(syncMagicV2))
	msg[4] = syncVersionV2
	msg[5] = byte(len(sender))
	binary.BigEndian.PutUint64(msg[6:14], uint64(ts))
	binary.BigEndian.PutUint64(msg[14:22], counter)
	copy(msg[22:38], connKey[:syncConnKeyLen])
	copy(msg[38:38+len(sender)], sender)

	mac := hmac.New(sha256.New, c.sharedKey)
	mac.Write(msg[:bodyLen])
	copy(msg[bodyLen:], mac.Sum(nil))
	return msg, nil
}

func (c *syncCodec) decodeConnEvent(pkt []byte) ([]byte, string, error) {
	if len(pkt) < syncHeaderLenV2+syncHMACLen {
		return nil, "", errInvalidPacket
	}
	if string(pkt[:4]) != syncMagicV2 || pkt[4] != syncVersionV2 {
		return nil, "", errInvalidPacket
	}

	senderLen := int(pkt[5])
	if senderLen <= 0 || senderLen > syncMaxSenderLen {
		return nil, "", errInvalidPacket
	}

	bodyLen := syncHeaderLenV2 + senderLen
	if len(pkt) != bodyLen+syncHMACLen {
		return nil, "", errInvalidPacket
	}

	calc := hmac.New(sha256.New, c.sharedKey)
	calc.Write(pkt[:bodyLen])
	expected := calc.Sum(nil)
	if subtle.ConstantTimeCompare(expected, pkt[bodyLen:]) != 1 {
		return nil, "", errInvalidMAC
	}

	ts := int64(binary.BigEndian.Uint64(pkt[6:14]))
	now := c.nowFn()
	pktTime := time.Unix(ts, 0)
	age := now.Sub(pktTime)
	if age < 0 {
		age = -age
	}
	if age > c.replayWindow {
		return nil, "", errExpiredPacket
	}

	sender := string(pkt[38 : 38+senderLen])
	counter := binary.BigEndian.Uint64(pkt[14:22])
	if err := c.replay.accept(sender, counter); err != nil {
		return nil, sender, err
	}

	key := make([]byte, syncConnKeyLen)
	copy(key, pkt[22:38])
	return key, sender, nil
}

func (r *replayGuard) accept(sender string, counter uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	last, ok := r.lastCounter[sender]
	if ok && counter <= last {
		return errReplayDetected
	}
	r.lastCounter[sender] = counter
	return nil
}

func (s *SyncRuntime) Close() error {
	if s == nil {
		return nil
	}
	s.cancel()
	if s.pc != nil {
		if err := s.pc.Close(); err != nil {
			return fmt.Errorf("close sync socket: %w", err)
		}
	}
	<-s.done
	return nil
}

func eventToPeersLoop(ctx context.Context, reader *ringbuf.Reader, pc net.PacketConn, peers []net.Addr, codec *syncCodec) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		reader.SetDeadline(time.Now().Add(1 * time.Second))
		rec, err := reader.Read()
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			slog.Warn("ringbuf read failed", "error", err)
			continue
		}
		if len(rec.RawSample) < syncConnKeyLen {
			continue
		}

		payload, err := codec.encodeConnEvent(rec.RawSample)
		if err != nil {
			slog.Warn("sync encode failed", "error", err)
			continue
		}
		for _, peer := range peers {
			if _, err := pc.WriteTo(payload, peer); err != nil {
				slog.Warn("sync send failed", "peer", peer.String(), "error", err)
			}
		}
	}
}

func peerToMapLoop(ctx context.Context, pc net.PacketConn, conntrack *ebpf.Map, codec *syncCodec) {
	buf := make([]byte, 2048)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_ = pc.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Warn("sync receive failed", "error", err)
			continue
		}

		key, sender, err := codec.decodeConnEvent(buf[:n])
		if err != nil {
			continue
		}
		if sender == codec.sender {
			continue
		}

		value := uint64(time.Now().UnixNano())
		if err := conntrack.Put(key, value); err != nil {
			slog.Warn("sync map insert failed", "error", err)
		}
	}
}
