package delivery

import (
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/config"
	"errors"
	"time"
)

// BuilderOffer is an authenticated builder's current capacity for one execution.
// Required memory includes both the build step and its per-execution daemon.
type BuilderOffer struct {
	ID                   string
	Name                 string
	HostType             config.HostType
	AvailableMemoryBytes int64
	RequiredMemoryBytes  int64
	AvailableCPUMillis   int64
	RequiredCPUMillis    int64
}

func (o BuilderOffer) validate() error {
	if o.ID == "" || o.RequiredMemoryBytes <= 0 || o.RequiredCPUMillis <= 0 || o.AvailableMemoryBytes < 0 || o.AvailableCPUMillis < 0 {
		return errors.New("builder offer requires an ID, positive resource requirements and non-negative capacity")
	}
	_, err := config.NormalizeHostType(string(o.HostType))
	return err
}
func (o BuilderOffer) fits() bool {
	return o.AvailableMemoryBytes >= o.RequiredMemoryBytes && o.AvailableCPUMillis >= o.RequiredCPUMillis
}
func preferredBuilderAvailable(ctx context.Context, tx *sql.Tx, offer BuilderOffer, now time.Time, ttl time.Duration) (bool, error) {
	if offer.HostType == config.HostIntermittent {
		return false, nil
	}
	var preferred bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM builder_workers WHERE id<>$1 AND host_type='intermittent' AND NOT drained AND current_build_id='' AND last_heartbeat_at>$2 AND required_memory_bytes>0 AND available_memory_bytes>=required_memory_bytes AND required_cpu_millis>0 AND available_cpu_millis>=required_cpu_millis)`, offer.ID, now.Add(-ttl)).Scan(&preferred)
	return preferred, err
}
