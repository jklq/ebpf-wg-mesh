package config

import (
	"fmt"
	"strings"
)

func NormalizeProfile(raw string) (Profile, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(ProfileProduction):
		return ProfileProduction, nil
	case string(ProfileDevelopment):
		return ProfileDevelopment, nil
	default:
		return "", fmt.Errorf("profile must be development or production")
	}
}

func applyProfile(raw Profile) (Profile, error) {
	return NormalizeProfile(string(raw))
}
