package mesh

import (
	"fmt"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func GeneratePrivateKey() (string, error) {
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return "", fmt.Errorf("generate wireguard key: %w", err)
	}
	return key.String(), nil
}

func PublicKey(privateKey string) (string, error) {
	key, err := wgtypes.ParseKey(privateKey)
	if err != nil {
		return "", fmt.Errorf("parse wireguard private key: %w", err)
	}
	return key.PublicKey().String(), nil
}
