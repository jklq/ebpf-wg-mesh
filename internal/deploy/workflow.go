package deploy

import (
	"cmp"
	"fmt"
	"slices"
)

type workflowMode uint8

const (
	freshMode workflowMode = 1 << iota
	upgradeMode
	changeMode
	automaticMode
	restoreMode
	deploymentModes = freshMode | upgradeMode | changeMode | automaticMode
	manualModes     = freshMode | upgradeMode | changeMode
)

// A phase is either a mandatory hook or a collection of native actions. Empty
// conditional phases remain dependency barriers. Algorithms decide which hosts
// and resources need actions; these declarations decide when they may run.
type workflowPhase struct {
	Name  string
	After []string
	Modes workflowMode
	Hook  string
}

var deploymentWorkflow = []workflowPhase{
	{"purchase", nil, deploymentModes, ""},
	{"stage", []string{"purchase"}, deploymentModes, ""},
	{"tools", []string{"stage"}, deploymentModes, ""},
	{"prior-tools", []string{"tools"}, deploymentModes, ""},
	{"protect", []string{"prior-tools"}, deploymentModes, "recovery-protect"},
	{"yield", []string{"protect"}, upgradeMode | changeMode | automaticMode, "reservations"},
	{"upgrade-prepare", []string{"yield"}, upgradeMode, "rolling-prepare"},
	{"pre-backup", []string{"upgrade-prepare"}, upgradeMode, "backup"},
	{"database-credentials", []string{"pre-backup"}, deploymentModes, "database-credentials"},
	{"database-runtime", []string{"database-credentials"}, manualModes, ""},
	{"database-finalize", []string{"database-runtime"}, upgradeMode, "database-finalize"},
	{"schema-expand", []string{"database-finalize"}, upgradeMode, ""},
	{"schema-overlap", []string{"schema-expand"}, upgradeMode, "schema-verify"},
	{"initialize", []string{"schema-overlap"}, freshMode, "database-init"},
	{"bootstrap", []string{"initialize"}, freshMode, "platform-bootstrap"},
	{"schedule", []string{"bootstrap"}, freshMode | changeMode | automaticMode, "backup-schedule"},
	{"enroll", []string{"schedule"}, freshMode, "reservations"},
	{"credentials", []string{"enroll"}, deploymentModes, "credentials"},
	{"registry", []string{"credentials"}, deploymentModes, ""},
	{"controlplane", []string{"registry"}, deploymentModes, ""},
	{"console", []string{"controlplane"}, deploymentModes, ""},
	{"builder", []string{"console"}, deploymentModes, ""},
	{"agent", []string{"builder"}, deploymentModes, ""},
	{"envoy", []string{"agent"}, deploymentModes, ""},
	{"database-verify", []string{"envoy"}, manualModes, "database-verify"},
	{"storage-verify", []string{"database-verify"}, manualModes, "storage-verify"},
	{"retire", []string{"storage-verify"}, deploymentModes, ""},
	{"schema-backfill", []string{"retire"}, upgradeMode, ""},
	{"schema-contract", []string{"schema-backfill"}, upgradeMode, ""},
	{"schema-final", []string{"schema-contract"}, upgradeMode, "schema-verify"},
	{"upgrade-schedule", []string{"schema-final"}, upgradeMode, "backup-schedule"},
	{"production-verify", []string{"upgrade-schedule"}, deploymentModes, "production-verify"},
	{"finalize", []string{"production-verify"}, deploymentModes, "recovery-finalize"},
	{"delete", []string{"finalize"}, manualModes, ""},
	{"first-backup", []string{"delete"}, freshMode, "backup"},
}

var restoreWorkflow = []workflowPhase{
	{"purchase", nil, restoreMode, ""},
	{"stage", []string{"purchase"}, restoreMode, ""},
	{"tools", []string{"stage"}, restoreMode, ""},
	{"verify-point", []string{"tools"}, restoreMode, "recovery-verify"},
	{"fence", []string{"verify-point"}, restoreMode, "recovery-fence"},
	{"stop-prior", []string{"fence"}, restoreMode, ""},
	{"database-credentials", []string{"stop-prior"}, restoreMode, "database-credentials"},
	{"database-runtime", []string{"database-credentials"}, restoreMode, ""},
	{"empty-destination", []string{"database-runtime"}, restoreMode, "recovery-database"},
	{"restore", []string{"empty-destination"}, restoreMode, "restore"},
	{"authority", []string{"restore"}, restoreMode, "recovery-authority"},
	{"registry", []string{"authority"}, restoreMode, ""},
	{"controlplane", []string{"registry"}, restoreMode, ""},
	{"console", []string{"controlplane"}, restoreMode, ""},
	{"agent", []string{"console"}, restoreMode, ""},
	{"envoy", []string{"agent"}, restoreMode, ""},
	{"builder", []string{"envoy"}, restoreMode, ""},
	{"inventory", []string{"builder"}, restoreMode, "recovery-inventory"},
	{"reserve", []string{"inventory"}, restoreMode, "recovery-reserve"},
	{"approve", []string{"reserve"}, restoreMode, "recovery-approve"},
	{"reconcile", []string{"approve"}, restoreMode, "recovery-reconcile"},
	{"checkpoints", []string{"reconcile"}, restoreMode, "recovery-checkpoints"},
	{"work", []string{"checkpoints"}, restoreMode, "recovery-work"},
	{"database-verify", []string{"work"}, restoreMode, "database-verify"},
	{"storage-verify", []string{"database-verify"}, restoreMode, "storage-verify"},
	{"production-verify", []string{"storage-verify"}, restoreMode, "production-verify"},
	{"schedule", []string{"production-verify"}, restoreMode, "backup-schedule"},
	{"backup", []string{"schedule"}, restoreMode, "backup"},
	{"resume", []string{"backup"}, restoreMode, "recovery-resume"},
}

func (p Plan) workflow() ([]workflowPhase, workflowMode) {
	if p.Recovery {
		return restoreWorkflow, restoreMode
	}
	if p.Automatic {
		return deploymentWorkflow, automaticMode
	}
	if p.Previous == nil {
		return deploymentWorkflow, freshMode
	}
	if Digest(p.Previous.Release) != Digest(p.Release) {
		return deploymentWorkflow, upgradeMode
	}
	return deploymentWorkflow, changeMode
}

// compileWorkflow uses the dependency graph, not declaration or collector order.
// Every persisted operation names its prerequisites, so retries cannot advance
// past an unverified dependency merely because an attempt was journaled.
func (p Plan) compileWorkflow(groups map[string][]Operation) ([]Operation, error) {
	definition, mode := p.workflow()
	return compileWorkflow(p, definition, mode, groups)
}

func compileWorkflow(p Plan, definition []workflowPhase, mode workflowMode, groups map[string][]Operation) ([]Operation, error) {
	definition = slices.Clone(definition)
	slices.SortFunc(definition, func(a, b workflowPhase) int { return cmp.Compare(a.Name, b.Name) })
	known := map[string]bool{}
	for _, phase := range definition {
		if known[phase.Name] {
			return nil, fmt.Errorf("duplicate lifecycle phase %s", phase.Name)
		}
		known[phase.Name] = true
	}
	for phase := range groups {
		if !known[phase] {
			return nil, fmt.Errorf("unknown lifecycle phase %s", phase)
		}
	}
	completed := map[string]bool{}
	last := map[string][]string{}
	var operations []Operation
	for len(completed) < len(definition) {
		advanced := false
		for _, phase := range definition {
			if completed[phase.Name] {
				continue
			}
			ready := true
			var dependencies []string
			for _, parent := range phase.After {
				if !known[parent] {
					return nil, fmt.Errorf("phase %s requires unknown phase %s", phase.Name, parent)
				}
				ready = ready && completed[parent]
				dependencies = append(dependencies, last[parent]...)
			}
			if !ready {
				continue
			}
			slices.Sort(dependencies)
			dependencies = slices.Compact(dependencies)
			steps := groups[phase.Name]
			if phase.Modes&mode == 0 {
				if len(steps) != 0 {
					return nil, fmt.Errorf("phase %s is forbidden in this lifecycle", phase.Name)
				}
			} else if phase.Hook != "" {
				if len(steps) != 0 {
					return nil, fmt.Errorf("phase %s is owned by its declared hook", phase.Name)
				}
				steps = []Operation{{Kind: "hook", Host: p.AdministrationHost, Hook: phase.Hook}}
			}
			for _, op := range steps {
				op.Phase, op.Requires = phase.Name, append([]string(nil), dependencies...)
				op.ID = ""
				op.ID = Digest(op)[:24]
				operations = append(operations, op)
				dependencies = []string{op.ID}
			}
			last[phase.Name] = dependencies
			completed[phase.Name], advanced = true, true
		}
		if !advanced {
			return nil, fmt.Errorf("lifecycle dependency graph contains a cycle")
		}
	}
	return operations, nil
}

func (p Plan) validateWorkflow() error {
	definition, mode := p.workflow()
	if mode == upgradeMode {
		if err := p.validateRollingUpgrade(); err != nil {
			return err
		}
	}
	generated := map[string]bool{}
	for _, phase := range definition {
		generated[phase.Name] = phase.Hook != ""
	}
	groups := map[string][]Operation{}
	for _, op := range p.Operations {
		if !generated[op.Phase] {
			groups[op.Phase] = append(groups[op.Phase], op)
		}
	}
	if mode == upgradeMode {
		if err := p.validateRollingSteps(groups); err != nil {
			return err
		}
	}
	for phase, role := range map[string]Role{"database-runtime": Database, "registry": Registry, "controlplane": ControlPlane, "console": Console, "builder": Builder, "agent": Agent, "envoy": Envoy} {
		steps := groups[phase]
		for n := 0; n < len(steps); {
			actions := runtimeActions
			if role == Builder && mode == upgradeMode && steps[n].Placement != nil && p.previouslyPlaced(*steps[n].Placement) {
				actions = []string{"builder-drain", "configure", "install", "builder-resume"}
			}
			if n+len(actions) > len(steps) {
				return fmt.Errorf("phase %s has an incomplete actor sequence", phase)
			}
			for index, action := range actions {
				op := steps[n+index]
				if op.Placement == nil || steps[n].Placement == nil {
					return fmt.Errorf("phase %s requires actor placements", phase)
				}
				if mode == upgradeMode && role == Database && action == "install" && p.previouslyPlaced(*op.Placement) && (op.Kind != "database-upgrade" || op.Hook != "database-upgrade") {
					return fmt.Errorf("existing database replicas require rolling health gates")
				}
				kind := op.Kind
				if kind == "hook" && op.Hook == action && (action == "builder-drain" || action == "builder-resume") {
					kind = action
				}
				if role == Database && (kind == "database-join" || (mode == upgradeMode && kind == "database-upgrade" && op.Hook == "database-upgrade")) {
					kind = "install"
				}
				host := op.Host
				if action == "builder-drain" || action == "builder-resume" {
					host = op.Placement.Host
					if op.Host != p.AdministrationHost {
						return fmt.Errorf("builder lifecycle requires the administration host")
					}
				}
				if kind != action || op.Placement == nil || op.Placement.Role != role || host != op.Placement.Host || steps[n].Placement == nil || op.Placement.Instance != steps[n].Placement.Instance {
					return fmt.Errorf("phase %s differs from its declared actor sequence", phase)
				}
			}
			n += len(actions)
		}
	}
	expected, err := p.compileWorkflow(groups)
	if err != nil {
		return err
	}
	if Digest(expected) != Digest(p.Operations) {
		return fmt.Errorf("operations differ from the declared lifecycle dependencies")
	}
	seen := map[string]bool{}
	for _, op := range p.Operations {
		if seen[op.ID] {
			return fmt.Errorf("duplicate operation identity %s", op.ID)
		}
		seen[op.ID] = true
		if contract, ok := ContractForHook(op.Hook); ok && contract.Activation {
			for _, gate := range contract.LiveGates {
				if !slices.ContainsFunc(p.Operations, func(candidate Operation) bool { return candidate.Hook == gate && seen[candidate.ID] }) {
					return fmt.Errorf("activation %s requires preceding %s verification", op.Hook, gate)
				}
			}
		}
	}
	return nil
}
