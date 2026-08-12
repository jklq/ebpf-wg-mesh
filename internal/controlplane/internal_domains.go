package controlplane

import "strings"

const internalDomainSuffix = "mesh.internal"

func internalServiceHostname(name, serviceID string) string {
	return internalServiceShortName(name, serviceID) + "." + internalDomainSuffix
}

func internalServiceShortName(name, serviceID string) string {
	var label strings.Builder
	label.Grow(len(name))
	separator := false
	for _, char := range strings.ToLower(strings.TrimSpace(name)) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			if separator && label.Len() > 0 {
				label.WriteByte('-')
			}
			separator = false
			label.WriteRune(char)
			continue
		}
		separator = true
	}
	shortName := strings.Trim(label.String(), "-")
	if shortName == "" {
		shortName = "service-" + compactServiceID(serviceID)
	}
	if len(shortName) > 63 {
		shortName = strings.TrimRight(shortName[:63], "-")
	}
	return shortName
}

func compactServiceID(serviceID string) string {
	var compact strings.Builder
	for _, char := range strings.ToLower(serviceID) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			compact.WriteRune(char)
			if compact.Len() == 12 {
				break
			}
		}
	}
	if compact.Len() == 0 {
		return "unknown"
	}
	return compact.String()
}
