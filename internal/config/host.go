package config

import (
	"fmt"
	"strings"
)

type HostType string

const (
	HostStable       HostType = "stable"
	HostIntermittent HostType = "intermittent"
)

func NormalizeHostType(raw string) (HostType, error) {
	switch HostType(strings.ToLower(strings.TrimSpace(raw))) {
	case "", HostStable:
		return HostStable, nil
	case HostIntermittent:
		return HostIntermittent, nil
	default:
		return "", fmt.Errorf("host type must be stable or intermittent")
	}
}
