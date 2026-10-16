package billing

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"sort"
	"time"
)

// Coverage describes how much of a rated window the meters actually saw.
// Gap and uncertain time is recorded on statements, never silently priced.
type Coverage struct {
	MeasuredMs  int64 `json:"measured_ms"`
	GapMs       int64 `json:"gap_ms"`
	UncertainMs int64 `json:"uncertain_ms"`
	Records     int64 `json:"records"`
}

// RatedLine is one project × meter exact amount before cent allocation.
type RatedLine struct {
	ProjectID   string
	ProjectName string
	Meter       Meter
	Quantity    *big.Int
	ExactCents  *big.Rat
}

// RatedPeriod is a workspace's rated usage for one period window.
type RatedPeriod struct {
	Lines      []RatedLine
	GrossExact *big.Rat
	Coverage   Coverage
	Rates      RateVersion
}

// loadRatesTx reads one immutable rate version.
func loadRatesTx(ctx context.Context, tx *sql.Tx, version int64, currency string) (RateVersion, error) {
	rows, err := tx.QueryContext(ctx, `SELECT meter, numerator::text, denominator::text FROM billing_rates WHERE version = $1 AND currency = $2`, version, currency)
	if err != nil {
		return RateVersion{}, err
	}
	defer rows.Close()
	out := RateVersion{Version: int(version), Currency: currency, Rates: map[Meter]Rate{}}
	for rows.Next() {
		var meter, num, den string
		if err := rows.Scan(&meter, &num, &den); err != nil {
			return RateVersion{}, err
		}
		n, okN := new(big.Int).SetString(num, 10)
		d, okD := new(big.Int).SetString(den, 10)
		if !okN || !okD {
			return RateVersion{}, fmt.Errorf("billing: corrupt rate for meter %s", meter)
		}
		out.Rates[Meter(meter)] = Rate{Num: n, Den: d}
	}
	if err := rows.Err(); err != nil {
		return RateVersion{}, err
	}
	for _, meter := range Meters {
		if _, ok := out.Rates[meter]; !ok {
			return RateVersion{}, fmt.Errorf("billing: rate version %d lacks meter %s", version, meter)
		}
	}
	return out, nil
}

type quantityKey struct {
	projectID string
	meter     Meter
}

// rateWindowTx rates a workspace's hourly usage in [from, to) at the given
// rate version. Quantities are the raw covered integrals: coverage-adjusted
// display scaling from the usage query layer never enters a bill, and
// retained (post-deletion) volume storage is operator-funded, not priced.
// Intervals belong to the hour bucket containing their start, exactly as
// usage queries assign them, so a period boundary falls on bucket edges.
func (s *Service) rateWindowTx(ctx context.Context, tx *sql.Tx, workspaceID string, rates RateVersion, from, to time.Time, storageOnly bool) (RatedPeriod, error) {
	rows, err := tx.QueryContext(ctx, `SELECT h.project_id,
			COALESCE(sum(h.cpu_usec), 0)::text,
			COALESCE(sum(h.memory_byte_ms), 0)::text,
			COALESCE(sum(h.volume_byte_ms - h.retained_volume_byte_ms), 0)::text,
			COALESCE(sum(h.egress_public_direct + h.egress_public_ingress), 0)::text,
			COALESCE(sum(h.measured_ms), 0), COALESCE(sum(h.gap_ms), 0),
			COALESCE(sum(h.uncertain_ms), 0), COALESCE(sum(h.records), 0)
		  FROM usage_hourly h
		  JOIN usage_attributions a ON a.resource_kind = h.resource_kind AND a.resource_id = h.resource_id
		 WHERE a.workspace_id = $1 AND h.hour >= $2 AND h.hour < $3
		 GROUP BY h.project_id`, workspaceID, from, to)
	if err != nil {
		return RatedPeriod{}, fmt.Errorf("rate usage: %w", err)
	}
	defer rows.Close()

	rated := RatedPeriod{GrossExact: new(big.Rat), Rates: rates}
	quantities := map[quantityKey]*big.Int{}
	var projectIDs []string
	for rows.Next() {
		var projectID, cpu, memory, volume, egress string
		var cov Coverage
		if err := rows.Scan(&projectID, &cpu, &memory, &volume, &egress,
			&cov.MeasuredMs, &cov.GapMs, &cov.UncertainMs, &cov.Records); err != nil {
			return RatedPeriod{}, err
		}
		rated.Coverage.MeasuredMs += cov.MeasuredMs
		rated.Coverage.GapMs += cov.GapMs
		rated.Coverage.UncertainMs += cov.UncertainMs
		rated.Coverage.Records += cov.Records
		projectIDs = append(projectIDs, projectID)
		for meter, raw := range map[Meter]string{
			MeterCPU: cpu, MeterMemory: memory, MeterVolume: volume, MeterEgress: egress,
		} {
			qty, ok := new(big.Int).SetString(raw, 10)
			if !ok {
				return RatedPeriod{}, fmt.Errorf("billing: corrupt %s quantity %q", meter, raw)
			}
			if qty.Sign() < 0 {
				qty.SetInt64(0)
			}
			quantities[quantityKey{projectID, meter}] = qty
		}
	}
	if err := rows.Err(); err != nil {
		return RatedPeriod{}, err
	}
	rows.Close()

	names, err := projectNamesTx(ctx, tx, projectIDs)
	if err != nil {
		return RatedPeriod{}, err
	}
	for _, projectID := range projectIDs {
		for _, meter := range Meters {
			if storageOnly && meter != MeterVolume {
				continue
			}
			qty := quantities[quantityKey{projectID, meter}]
			if qty == nil || qty.Sign() == 0 {
				continue
			}
			exact := rates.Rates[meter].Price(qty)
			rated.GrossExact.Add(rated.GrossExact, exact)
			rated.Lines = append(rated.Lines, RatedLine{
				ProjectID: projectID, ProjectName: names[projectID], Meter: meter,
				Quantity: qty, ExactCents: exact,
			})
		}
	}
	sort.Slice(rated.Lines, func(i, j int) bool {
		if rated.Lines[i].ProjectID != rated.Lines[j].ProjectID {
			return rated.Lines[i].ProjectID < rated.Lines[j].ProjectID
		}
		return rated.Lines[i].Meter < rated.Lines[j].Meter
	})
	return rated, nil
}

// projectNamesTx resolves display names for projects that still exist;
// deleted projects keep their ID as the statement label.
func projectNamesTx(ctx context.Context, tx *sql.Tx, projectIDs []string) (map[string]string, error) {
	names := make(map[string]string, len(projectIDs))
	for _, id := range projectIDs {
		names[id] = id
	}
	if len(projectIDs) == 0 {
		return names, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, name FROM projects WHERE id = ANY($1)`, projectIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		names[id] = name
	}
	return names, rows.Err()
}

// watermarkTx returns the oldest sealed-through instant across producers of
// agents holding current resources for the workspace, mirroring the usage
// query watermark so estimates can name their own freshness.
func watermarkTx(ctx context.Context, tx *sql.Tx, workspaceID string) (sql.NullTime, error) {
	var watermark sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT min(p.sealed_through)
		  FROM usage_producers p
		 WHERE p.state = 'active' AND EXISTS (
			SELECT 1 FROM usage_attributions a
			 WHERE a.agent_id = p.agent_id AND a.workspace_id = $1 AND a.ended_at IS NULL)`, workspaceID).Scan(&watermark)
	return watermark, err
}
