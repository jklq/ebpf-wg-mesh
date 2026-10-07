package recovery

import (
	"context"
	"fmt"
	"time"
)

type Attempt struct {
	At     time.Time `json:"at"`
	Report Report    `json:"report"`
}

func (s Service) RecordFailure(ctx context.Context, installation string, report Report) error {
	now := time.Now().UTC()
	_, err := s.putJSON(ctx, s.Prefix+"/reports/"+installation+"/"+now.Format("20060102T150405.000000000"), Attempt{now, report}, now.Add(Retention))
	return err
}

func (s Service) LatestAttempt(ctx context.Context, installation string) (Attempt, error) {
	objects, err := s.Storage.Versions(ctx, s.Prefix+"/reports/"+installation+"/")
	if err != nil {
		return Attempt{}, err
	}
	var latest Attempt
	for _, o := range objects {
		var attempt Attempt
		if err := s.readJSON(ctx, o, &attempt); err != nil {
			return Attempt{}, fmt.Errorf("read recovery attempt report: %w", err)
		}
		if attempt.At.After(latest.At) {
			latest = attempt
		}
	}
	return latest, nil
}
