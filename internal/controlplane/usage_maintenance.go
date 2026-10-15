package controlplane

import (
	"context"
	"log/slog"
	"time"
)

// usageMaintenanceLoop prepares raw usage partitions and applies usage
// retention on the live owner.
func (s *Server) usageMaintenanceLoop(ctx context.Context) {
	maintain := func() {
		result, err := s.usage.Maintain(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("usage maintenance failed", "error", err)
			}
			return
		}
		if result.DroppedPartitions > 0 || result.HourlyRows > 0 || result.Attributions > 0 {
			slog.Info("usage retention applied", "dropped_partitions", result.DroppedPartitions, "hourly_rows", result.HourlyRows, "attributions", result.Attributions)
		}
	}
	maintain()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			maintain()
		}
	}
}
