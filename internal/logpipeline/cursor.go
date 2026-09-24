package logpipeline

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// EncodeCursor builds an opaque page token from the last
// returned record's ordering key: (observed_at, line_id) for lines,
// (window_start, gap_id) for gaps.
func EncodeCursor(observedAt time.Time, lineID string) string {
	raw := make([]byte, 8+4+len(lineID))
	binary.BigEndian.PutUint64(raw[:8], uint64(observedAt.UTC().UnixNano()))
	binary.BigEndian.PutUint32(raw[8:12], uint32(len(lineID)))
	copy(raw[12:], lineID)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeCursor parses a token from EncodeCursor. An empty token is
// the start of the range and decodes to the zero time and empty ID.
func DecodeCursor(token string) (time.Time, string, error) {
	if token == "" {
		return time.Time{}, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) < 12 {
		return time.Time{}, "", errors.New("invalid page token")
	}
	nanos := int64(binary.BigEndian.Uint64(raw[:8]))
	idLen := int(binary.BigEndian.Uint32(raw[8:12]))
	if idLen < 0 || 12+idLen != len(raw) {
		return time.Time{}, "", errors.New("invalid page token")
	}
	observedAt := time.Unix(0, nanos).UTC()
	if observedAt.Year() < 2000 || observedAt.Year() > 2200 {
		return time.Time{}, "", fmt.Errorf("invalid page token timestamp")
	}
	return observedAt, string(raw[12:]), nil
}
