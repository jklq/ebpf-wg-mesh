package deploy

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

var rollingHooks = []string{"database-upgrade", "database-finalize", "builder-drain", "builder-resume", "rolling-prepare", "schema-verify"}

func (p Plan) previouslyPlaced(pl Placement) bool {
	if p.Previous == nil {
		return false
	}
	for _, old := range p.Previous.Placements {
		if old == pl {
			return true
		}
	}
	return false
}

func (p Plan) validateRollingSteps(groups map[string][]Operation) error {
	change := p.Release.SchemaChanges[p.Previous.Release.ID]
	for kind, hook := range map[string]Hook{"schema-expand": change.Expand, "schema-backfill": change.Backfill, "schema-contract": change.Contract} {
		steps := groups[kind]
		if !hookValid(hook) {
			if len(steps) != 0 {
				return fmt.Errorf("undeclared %s operation", kind)
			}
			continue
		}
		if len(steps) != 1 || steps[0].Kind != kind || steps[0].Host != p.AdministrationHost || steps[0].Hook != "" || steps[0].Placement != nil {
			return fmt.Errorf("schema change requires its declared %s operation", kind)
		}
	}
	for _, pl := range p.Placements {
		phase := string(pl.Role)
		if pl.Role == Database {
			phase = "database-runtime"
		}
		installs := 0
		for _, op := range groups[phase] {
			if op.Placement != nil && *op.Placement == pl && slices.Contains([]string{"install", "database-upgrade", "database-join"}, op.Kind) {
				installs++
			}
		}
		if installs != 1 {
			return fmt.Errorf("rolling replica %s requires exactly one verified install", pl.Instance)
		}
	}
	for _, old := range p.Previous.Placements {
		if slices.Contains(p.Placements, old) {
			continue
		}
		actions := statelessRetirementActions
		if old.Role == Database {
			actions = databaseRetirementActions
		}
		for _, action := range actions {
			if !slices.ContainsFunc(groups["retire"], func(op Operation) bool { return op.Kind == action && op.Placement != nil && *op.Placement == old }) {
				return fmt.Errorf("old replica %s requires %s before schema cleanup", old.Instance, action)
			}
		}
	}
	return nil
}

func (p Plan) validateRollingUpgrade() error {
	if p.Previous == nil {
		return nil
	}
	old := p.Previous.Release
	if !p.Release.SchemaCompatibility {
		return fmt.Errorf("rolling release must declare schema compatibility and provide rolling lifecycle hooks")
	}
	for _, name := range rollingHooks {
		if !hookValid(p.Release.Hooks[name]) {
			return fmt.Errorf("rolling release requires %s command and verification", name)
		}
	}
	if old.Protocol != p.Release.Protocol || old.Configuration != p.Release.Configuration {
		return fmt.Errorf("rolling upgrades require compatible configuration and protocol versions")
	}
	if cockroachReleaseMajor(old.Dependencies["cockroachdb"]) != cockroachReleaseMajor(p.Release.Dependencies["cockroachdb"]) {
		members := func(placements []Placement) []string {
			var ids []string
			for _, pl := range placements {
				if pl.Role == Database {
					ids = append(ids, pl.Instance)
				}
			}
			slices.Sort(ids)
			return ids
		}
		if !slices.Equal(members(p.Previous.Placements), members(p.Placements)) {
			return fmt.Errorf("change CockroachDB membership in a separate plan before a major-version upgrade")
		}
	}
	changed := old.Schema != p.Release.Schema || old.ConsoleSchema != p.Release.ConsoleSchema
	if p.Release.Schema < old.Schema || p.Release.ConsoleSchema < old.ConsoleSchema {
		return fmt.Errorf("schema downgrades require restore")
	}
	change, declared := p.Release.SchemaChanges[old.ID]
	if changed && !declared {
		return fmt.Errorf("schema change requires online expand/backfill or a later contract release from %s; offline conversions cannot roll", old.ID)
	}
	if declared {
		if !changed {
			return fmt.Errorf("schema changes must publish a new schema version")
		}
		if hookValid(change.Contract) {
			minimum, consoleMinimum := p.Release.minimumSchemas()
			if minimum > old.Schema || consoleMinimum > old.ConsoleSchema {
				return fmt.Errorf("contract release must run on the previous schemas until all old replicas retire")
			}
		} else {
			if !old.SchemaCompatibility {
				return fmt.Errorf("first deploy a schema-compatible release before online expansion")
			}
			if !hookValid(change.Expand) || !hookValid(change.Backfill) {
				return fmt.Errorf("online schema expansion requires both expand and backfill verification")
			}
		}
	}
	return validateCockroachUpgrade(old.Dependencies["cockroachdb"], p.Release.Dependencies["cockroachdb"])
}

// Only adjacent major versions are admitted here. A skipped Innovation release
// requires a separately reviewed intermediate release rather than guessing
// support from an arbitrary version string.
func validateCockroachUpgrade(from, to string) error {
	parse := func(v string) ([3]int, error) {
		var parts [3]int
		fields := strings.Split(strings.TrimPrefix(v, "v"), ".")
		if len(fields) != 3 {
			return parts, fmt.Errorf("CockroachDB must pin a stable major.minor.patch version")
		}
		for n, field := range fields {
			x, err := strconv.Atoi(field)
			if err != nil || x < 0 {
				return parts, fmt.Errorf("invalid CockroachDB version %q", v)
			}
			parts[n] = x
		}
		return parts, nil
	}
	a, err := parse(from)
	if err != nil {
		return err
	}
	b, err := parse(to)
	if err != nil {
		return err
	}
	if a[0] == b[0] && a[1] == b[1] && b[2] >= a[2] {
		return nil
	}
	if (a[0] == b[0] && b[1] == a[1]+1) || (b[0] == a[0]+1 && a[1] == 4 && b[1] == 1) {
		return nil
	}
	return fmt.Errorf("unsupported CockroachDB rolling version transition %s -> %s; use adjacent major releases or a restore for downgrade", from, to)
}

func cockroachReleaseMajor(version string) string {
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	if len(parts) < 2 {
		return ""
	}
	return strings.Join(parts[:2], ".")
}
