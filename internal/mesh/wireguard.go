package mesh

import (
	"fmt"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// GeneratePrivateKey returns a fresh WireGuard private key in the text form
// stored in agent config.
func GeneratePrivateKey() (string, error) {
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return "", fmt.Errorf("generate wireguard key: %w", err)
	}
	return key.String(), nil
}

// PublicKey derives the public key peers must configure for privateKey.
func PublicKey(privateKey string) (string, error) {
	key, err := wgtypes.ParseKey(privateKey)
	if err != nil {
		return "", fmt.Errorf("parse wireguard private key: %w", err)
	}
	return key.PublicKey().String(), nil
}
