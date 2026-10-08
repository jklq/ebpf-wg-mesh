//go:build !linux

package builder

import (
	"context"
	"ebof-wg-mesh/internal/config"
	"fmt"
)

func ImportRuntimeImage(context.Context, config.BuilderConfig, string, string) error {
	return fmt.Errorf("native builder image import requires Linux")
}
func VerifyRuntimeImage(context.Context, config.BuilderConfig, string) error {
	return fmt.Errorf("native builder image inspection requires Linux")
}
