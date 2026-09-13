package process

import (
	"errors"
	"testing"
)

func mustCatalog(t *testing.T) Catalog {
	t.Helper()
	c, err := DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDefaultCatalogHasEveryKind(t *testing.T) {
	c := mustCatalog(t)
	for _, k := range []string{"feature", "bug", "incident", "chore"} {
		if _, ok := c[k]; !ok {
			t.Errorf("missing template %q", k)
		}
	}
}

func TestFeatureWithoutDeployEndsAtApproved(t *testing.T) {
	m, err := NewMachine(mustCatalog(t)["feature"], nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Phases()[len(m.Phases())-1].Name; got != "approved" {
		t.Fatalf("last phase without deploy = %q, want approved", got)
	}
	if !m.IsTerminal("approved") {
		t.Fatal("approved should be terminal without deploy")
	}
	if _, err := m.Phase("release"); !errors.Is(err, ErrUnknownPhase) {
		t.Fatalf("release should be inactive without deploy, got %v", err)
	}
}

func TestFeatureWithDeployReachesMonitoring(t *testing.T) {
	m, err := NewMachine(mustCatalog(t)["feature"], []string{"deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsTerminal("monitoring") {
		t.Fatal("monitoring should be terminal with deploy")
	}
	next, err := m.Next("approved")
	if err != nil || next.Name != "release" {
		t.Fatalf("after approved with deploy: %v %v", next.Name, err)
	}
}

func TestEveryPhaseHasAnExit(t *testing.T) {
	// "A state with no exit is a bug": every non-terminal phase must have a Next,
	// and every phase must be leavable by a human, an automation or a timeout.
	for kind, tpl := range mustCatalog(t) {
		m, err := NewMachine(tpl, []string{"deploy"})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range m.Phases() {
			if m.IsTerminal(p.Name) {
				continue
			}
			if _, err := m.Next(p.Name); err != nil {
				t.Errorf("%s/%s has no next phase: %v", kind, p.Name, err)
			}
			if p.Gate == GateAuto && p.Timeout == 0 && p.Owner != OwnerHuman {
				t.Errorf("%s/%s is auto-gated with no timeout and no human owner: dead end", kind, p.Name)
			}
		}
	}
}

func TestRollbackOnlyBackwardAndBounded(t *testing.T) {
	m, err := NewMachine(mustCatalog(t)["feature"], nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CanRollback("implementation", "planning", 0); err != nil {
		t.Fatalf("backward rollback should be allowed: %v", err)
	}
	if err := m.CanRollback("planning", "implementation", 0); !errors.Is(err, ErrNotBackward) {
		t.Fatalf("forward rollback should fail with ErrNotBackward, got %v", err)
	}
	if err := m.CanRollback("planning", "planning", 0); !errors.Is(err, ErrNotBackward) {
		t.Fatalf("same-phase rollback should fail, got %v", err)
	}
	if err := m.CanRollback("implementation", "planning", m.MaxRollbacks()); !errors.Is(err, ErrRollbackCeiling) {
		t.Fatalf("ceiling should block, got %v", err)
	}
}

func TestOverrideReplacesKind(t *testing.T) {
	c := mustCatalog(t)
	custom := Template{Kind: "chore", MaxRollbacks: 5, Phases: []Phase{
		{Name: "do", Owner: OwnerHuman, Gate: GateHuman},
	}}
	merged := c.Override(Catalog{"chore": custom})
	if merged["chore"].MaxRollbacks != 5 || len(merged["feature"].Phases) == 0 {
		t.Fatal("override should replace chore and keep feature")
	}
}

func TestValidateRejectsRunnerWithoutRole(t *testing.T) {
	bad := Template{Kind: "x", Phases: []Phase{{Name: "a", Owner: OwnerRunner, Gate: GateHuman}}}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}
