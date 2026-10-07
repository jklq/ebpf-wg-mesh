//go:build !linux

package productionops

import (
	"context"
	"fmt"
)

func processAdmission(context.Context, string, string, string, string) error {
	return fmt.Errorf("systemd admission inspection requires Linux")
}
