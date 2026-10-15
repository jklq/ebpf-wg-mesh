package agent

import "ebof-wg-mesh/internal/volumestore"

// volumeUsageSource adapts the volume store to metering's exact-usage seam.
type volumeUsageSource struct{ store *volumestore.Store }

func (s volumeUsageSource) Inventory() ([]string, error) { return s.store.Inventory() }
func (s volumeUsageSource) UsedBytes(id string) (int64, error) {
	return s.store.Usage(id)
}
