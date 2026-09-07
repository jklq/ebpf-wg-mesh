package delivery

import (
	"crypto/sha256"
	"strings"
)

func BootstrapTokenHash(token string) [sha256.Size]byte {
	return sha256.Sum256([]byte(strings.TrimSpace(token)))
}
