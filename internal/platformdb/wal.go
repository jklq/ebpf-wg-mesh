package platformdb

import (
	"fmt"
	"strconv"
	"strings"
)

// WAL segment names are 24 hex digits: timeline, log, segment. Ordering is
// only meaningful within one timeline, which is all a single-primary
// platform database produces between restores (each restore starts a new
// timeline and a new backup chain).

// ParseWALSegment splits a segment name into timeline and sequence value.
func ParseWALSegment(name string) (timeline int, sequence uint64, err error) {
	base := name
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	// Drop compression/partial suffixes pgBackRest may append in the repo,
	// and the .backup label suffix the archiver reports.
	if len(base) > 24 && (base[24] == '.' || base[24] == '-') {
		base = base[:24]
	}
	if i := strings.IndexByte(base, '-'); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimSuffix(base, ".partial")
	if len(base) != 24 {
		return 0, 0, fmt.Errorf("invalid WAL segment name %q", name)
	}
	tl, err := strconv.ParseUint(base[:8], 16, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid WAL segment name %q", name)
	}
	seq, err := strconv.ParseUint(base[8:], 16, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid WAL segment name %q", name)
	}
	return int(tl), seq, nil
}

// CompareWALSegments orders two segments on the same timeline. Segments on
// different timelines order by timeline first, which is sufficient for
// selecting the archive range a recovery point freezes.
func CompareWALSegments(a, b string) (int, error) {
	ta, sa, err := ParseWALSegment(a)
	if err != nil {
		return 0, err
	}
	tb, sb, err := ParseWALSegment(b)
	if err != nil {
		return 0, err
	}
	switch {
	case ta != tb:
		if ta < tb {
			return -1, nil
		}
		return 1, nil
	case sa != sb:
		if sa < sb {
			return -1, nil
		}
		return 1, nil
	default:
		return 0, nil
	}
}
