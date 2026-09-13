package process

import (
	"errors"
	"fmt"
)

var (
	ErrUnknownPhase   = errors.New("unknown phase")
	ErrTerminalPhase  = errors.New("phase is terminal")
	ErrNotBackward    = errors.New("rollback target is not an earlier phase")
	ErrRollbackCeiling = errors.New("rollback ceiling reached; a human must unblock")
)

// Machine answers questions about phase order for one template under a set of
// available plugin capabilities. Phases whose Requires capability is missing are
// skipped as if they did not exist.
type Machine struct {
	template Template
	active   []Phase
	index    map[string]int
}

// NewMachine builds a machine for a template. capabilities lists what plugins the
// project has enabled (e.g. "deploy"); phases requiring an absent capability are inactive.
func NewMachine(t Template, capabilities []string) (*Machine, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	caps := map[string]bool{}
	for _, c := range capabilities {
		caps[c] = true
	}
	m := &Machine{template: t, index: map[string]int{}}
	for _, p := range t.Phases {
		if p.Requires != "" && !caps[p.Requires] {
			continue
		}
		m.index[p.Name] = len(m.active)
		m.active = append(m.active, p)
	}
	if len(m.active) == 0 {
		return nil, fmt.Errorf("template %q has no active phases", t.Kind)
	}
	return m, nil
}

// Kind returns the template kind.
func (m *Machine) Kind() string { return m.template.Kind }

// Phases returns the active phases in order.
func (m *Machine) Phases() []Phase { return m.active }

// First returns the entry phase.
func (m *Machine) First() Phase { return m.active[0] }

// Phase looks up an active phase by name.
func (m *Machine) Phase(name string) (Phase, error) {
	i, ok := m.index[name]
	if !ok {
		return Phase{}, fmt.Errorf("%w: %q", ErrUnknownPhase, name)
	}
	return m.active[i], nil
}

// IsTerminal reports whether name is the last active phase.
func (m *Machine) IsTerminal(name string) bool {
	i, ok := m.index[name]
	return ok && i == len(m.active)-1
}

// Next returns the phase after name.
func (m *Machine) Next(name string) (Phase, error) {
	i, ok := m.index[name]
	if !ok {
		return Phase{}, fmt.Errorf("%w: %q", ErrUnknownPhase, name)
	}
	if i == len(m.active)-1 {
		return Phase{}, fmt.Errorf("%w: %q", ErrTerminalPhase, name)
	}
	return m.active[i+1], nil
}

// CanRollback reports whether moving from → to is a legal backward move and whether the
// rollback ceiling for the target phase allows it. rollbacksSoFar counts prior rollbacks
// into `to`.
func (m *Machine) CanRollback(from, to string, rollbacksSoFar int) error {
	fi, ok := m.index[from]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownPhase, from)
	}
	ti, ok := m.index[to]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownPhase, to)
	}
	if ti >= fi {
		return fmt.Errorf("%w: %q → %q", ErrNotBackward, from, to)
	}
	if m.template.MaxRollbacks > 0 && rollbacksSoFar >= m.template.MaxRollbacks {
		return fmt.Errorf("%w: %q already rolled back %d times", ErrRollbackCeiling, to, rollbacksSoFar)
	}
	return nil
}

// MaxRollbacks returns the template ceiling (0 = unlimited).
func (m *Machine) MaxRollbacks() int { return m.template.MaxRollbacks }
