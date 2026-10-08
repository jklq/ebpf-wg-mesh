package deploy

import (
	"slices"
	"testing"
)

func TestLifecycleGraphIgnoresDeclarationOrderAndRejectsCycles(t *testing.T) {
	i, r, inv := fixture(3)
	p := build(t, i, r, State{}, inv, false)
	definition, mode := p.workflow()
	definition = slices.Clone(definition)
	groups := map[string][]Operation{}
	generated := map[string]bool{}
	for _, phase := range definition {
		generated[phase.Name] = phase.Hook != ""
	}
	for _, op := range p.Operations {
		if !generated[op.Phase] {
			groups[op.Phase] = append(groups[op.Phase], op)
		}
	}
	slices.Reverse(definition)
	operations, err := compileWorkflow(p, definition, mode, groups)
	if err != nil || Digest(operations) != Digest(p.Operations) {
		t.Fatalf("dependency ordering changed when declarations moved: %v", err)
	}
	for n := range definition {
		if definition[n].Name == "purchase" {
			definition[n].After = []string{"first-backup"}
		}
	}
	if _, err := compileWorkflow(p, definition, mode, groups); err == nil {
		t.Fatal("dependency cycle was accepted")
	}
}

func TestPlanRejectsMissingVerificationAndEarlyDatabaseStartup(t *testing.T) {
	i, r, inv := fixture(3)
	for _, mutate := range []func(*Plan){
		func(p *Plan) {
			p.Operations = slices.DeleteFunc(p.Operations, func(op Operation) bool { return op.Hook == "production-verify" })
		},
		func(p *Plan) {
			for n := range p.Operations {
				if p.Operations[n].Phase == "database-runtime" && p.Operations[n].Kind == "configure" {
					p.Operations[n], p.Operations[n+1] = p.Operations[n+1], p.Operations[n]
					return
				}
			}
		},
		func(p *Plan) {
			for n := range p.Operations {
				if len(p.Operations[n].Requires) > 0 {
					p.Operations[n].Requires = nil
					return
				}
			}
		},
	} {
		p := build(t, i, r, State{}, inv, false)
		mutate(&p)
		p.ID = p.digest()
		if err := p.Validate(); err == nil {
			t.Fatal("integrity-valid plan bypassed the lifecycle declaration")
		}
	}
}

func TestVerificationContractRejectsMissingProofsAndWrongAuthority(t *testing.T) {
	i, r, inv := fixture(3)
	p := build(t, i, r, State{}, inv, false)
	c, _ := ContractForHook("production-verify")
	e := Evidence{Recovery: &RecoveryReceipt{Installation: i.ID, Generation: p.Generation, Checks: map[string]bool{}}}
	for _, proof := range c.Checks {
		e.Recovery.Checks[proof] = true
	}
	if err := ValidateHookEvidence("production-verify", p, e); err != nil {
		t.Fatal(err)
	}
	delete(e.Recovery.Checks, c.Checks[0])
	if err := ValidateHookEvidence("production-verify", p, e); err == nil {
		t.Fatal("missing required native proof accepted")
	}
	e.Recovery.Checks[c.Checks[0]] = true
	e.Recovery.Generation = "old-authority"
	if err := ValidateHookEvidence("production-verify", p, e); err == nil {
		t.Fatal("proof from another authority accepted")
	}
	if err := ValidateHookEvidence("database-verify", p, Evidence{}); err == nil {
		t.Fatal("empty database evidence accepted")
	}
}
