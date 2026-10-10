package productionops

import (
	"context"
	"fmt"
	"os"

	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/schemacompat"
)

func (r *Runner) verifySchemaCompatibility(ctx context.Context) error {
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	phase := ""
	for _, op := range r.Plan.Operations {
		if op.ID == os.Getenv("PLATFORM_OPERATION") {
			phase = op.Phase
		}
	}
	if phase != "schema-overlap" && phase != "schema-final" {
		return fmt.Errorf("schema verification requires its deployment operation")
	}
	for _, item := range []struct {
		schema                  string
		current, minimum, prior int
	}{
		{"public", r.Plan.Release.Schema, minimumSchema(r.Plan.Release, false), priorSchema(r.Plan.Previous, false)},
		{r.Config.Console.Schema, r.Plan.Release.ConsoleSchema, minimumSchema(r.Plan.Release, true), priorSchema(r.Plan.Previous, true)},
	} {
		status, err := schemacompat.Read(ctx, db, item.schema)
		if err != nil {
			return err
		}
		if !status.Supports(item.current, item.minimum) {
			return fmt.Errorf("%s schema is incompatible with the new release", item.schema)
		}
		if phase == "schema-overlap" {
			if !status.Supports(item.prior, item.prior) {
				return fmt.Errorf("%s schema does not admit surviving old replicas", item.schema)
			}
		} else if status.Version != item.current {
			return fmt.Errorf("%s schema change has not completed", item.schema)
		}
	}
	return nil
}

func minimumSchema(r deploy.Release, console bool) int {
	if console {
		if r.MinConsoleSchema > 0 {
			return r.MinConsoleSchema
		}
		return r.ConsoleSchema
	}
	if r.MinSchema > 0 {
		return r.MinSchema
	}
	return r.Schema
}

func priorSchema(p *deploy.AppliedDeployment, console bool) int {
	if p == nil {
		return 0
	}
	if console {
		return p.Release.ConsoleSchema
	}
	return p.Release.Schema
}
