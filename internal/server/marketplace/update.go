package marketplace

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// Updater is an installer that brings what it installed to a new version in place, minding what a
// person changed of it since. Parts are named as Installed.State names them.
type Updater interface {
	// Current is what of the installation a person may have changed, as it is now.
	Current(ctx context.Context, tx *db.Queries, inst db.Installation) (map[string]string, error)
	// Incoming is what pkg would install of those parts.
	Incoming(pkg Package) (map[string]string, error)
	// Update brings it to in.Package; keep names the parts whose local version stays.
	Update(ctx context.Context, tx *db.Queries, inst db.Installation, in Input, keep map[string]bool) (Installed, error)
}

// Ways to settle local changes.
const (
	KeepLocal  = "keep_local"  // the new version, but the parts a person changed stay as they are
	TakeTheirs = "take_theirs" // the new version whole; local changes go
)

// UpdateRequest asks for an installation to be brought to another version.
type UpdateRequest struct {
	// Version and Checksum are the version the person previewed and accepted, in the
	// installation's registry.
	Version  string
	Checksum string
	// Accepted are the permissions accepted now; with what was accepted before, they must hold
	// every permission the new version asks for.
	Accepted []string
	// Resolve settles parts a person changed since installing (KeepLocal, TakeTheirs); empty
	// refuses to touch them.
	Resolve string
	// Downgrade allows a version older than the one installed: never by accident.
	Downgrade bool
}

// LocalChange is one part a person changed since it was installed: their change, and what the new
// version would change, both as diffs from what was installed.
type LocalChange struct {
	Part     string
	Local    string
	Incoming string
}

// Preview is what an update would do.
type Preview struct {
	From, To string
	Checksum string // of To
	// Added are the permissions the new version asks for that were never accepted; Removed those
	// it no longer asks for.
	Added, Removed []string
	Local          []LocalChange
}

// LocalChangesError is an update that would overwrite what a person changed, until they say
// which way to settle it.
type LocalChangesError struct{ Changes []LocalChange }

func (e *LocalChangesError) Error() string {
	parts := make([]string, 0, len(e.Changes))
	for _, c := range e.Changes {
		parts = append(parts, c.Part)
	}
	return fmt.Sprintf("%s: changed here since it was installed: %s; keep them (%s) or take the new version's (%s)",
		ErrConflict, strings.Join(parts, ", "), KeepLocal, TakeTheirs)
}

// Unwrap lets errors.Is find ErrConflict.
func (e *LocalChangesError) Unwrap() error { return ErrConflict }

// Preview says what bringing an installation to version (empty: the newest) would do: the
// permissions it adds and drops, and what a person changed that it would touch. Nothing changes.
func (s *Installations) Preview(ctx context.Context, p auth.Principal, id pgtype.UUID, version string) (db.Installation, Preview, error) {
	inst, pv, _, _, err := s.preview(ctx, p, id, version)
	return inst, pv, err
}

func (s *Installations) preview(ctx context.Context, p auth.Principal, id pgtype.UUID, version string) (db.Installation, Preview, Package, Updater, error) {
	inst, err := s.Get(ctx, p, id)
	if err != nil {
		return inst, Preview{}, Package{}, nil, err
	}
	var pv Preview
	var pkg Package
	var up Updater
	err = s.tx(ctx, func(q *db.Queries) error {
		var err error
		pv, pkg, up, err = s.plan(ctx, q, p, inst, version)
		return err
	})
	return inst, pv, pkg, up, err
}

// plan works out what bringing inst to version would do, reading what a person changed through q:
// in the transaction that applies it, nothing can change between what is shown and what is done.
func (s *Installations) plan(ctx context.Context, q *db.Queries, p auth.Principal, inst db.Installation, version string) (Preview, Package, Updater, error) {
	up, ok := s.installers[inst.Kind].(Updater)
	if !ok {
		return Preview{}, Package{}, nil, invalid("a %s is not updated in place; remove it and install the new version", inst.Kind)
	}
	v, err := s.choose(ctx, p, inst.Kind, inst.Name, version, inst.RegistryID)
	if err != nil {
		return Preview{}, Package{}, nil, err
	}
	pkg, err := s.packages.Package(ctx, v.RegistryID, inst.Kind, inst.Name, v.Manifest.Version)
	if err != nil {
		return Preview{}, Package{}, nil, err
	}
	var rec installedRecord
	_ = json.Unmarshal(inst.Installed, &rec)
	var accepted []string
	_ = json.Unmarshal(inst.AcceptedPermissions, &accepted)
	pv := Preview{From: inst.Version, To: pkg.Item.Version, Checksum: pkg.Item.Checksum, Added: NewPermissions(pkg.Manifest.Permissions, accepted),
		Removed: NewPermissions(rec.Manifest.Permissions, pkg.Manifest.Permissions)}
	incoming, err := up.Incoming(pkg)
	if err != nil {
		return pv, pkg, up, err
	}
	current, err := up.Current(ctx, q, inst)
	if err != nil {
		return pv, pkg, up, err
	}
	for _, part := range sortedKeys(current) {
		now := current[part]
		was, known := rec.State[part]
		if !known || same(was, now) {
			continue // a part nothing was recorded of has no "as installed" to differ from
		}
		pv.Local = append(pv.Local, LocalChange{Part: part,
			Local:    Diff(was, now, part+" as installed ("+inst.Version+")", part+" as it is now"),
			Incoming: Diff(was, incoming[part], part+" as installed ("+inst.Version+")", part+" in "+pkg.Item.Version)})
	}
	return pv, pkg, up, nil
}

// Update brings an installation to another version, never by itself: a person asks, accepts what
// the new version adds, and says what becomes of their own changes. The installation is locked
// and what a person changed is read again in the same transaction that applies the update.
func (s *Installations) Update(ctx context.Context, p auth.Principal, id pgtype.UUID, req UpdateRequest) (db.Installation, error) {
	if req.Version == "" || req.Checksum == "" {
		return db.Installation{}, invalid("an update names the version and the checksum the person accepted")
	}
	if inst, err := s.Get(ctx, p, id); err != nil {
		return inst, err
	} else if err := s.canManage(ctx, p, inst.ProjectID); err != nil {
		return inst, err
	}
	var out db.Installation
	var undo undoer
	var err error
	defer func() {
		if err != nil {
			undo.run()
		}
	}()
	err = s.tx(ctx, func(q *db.Queries) error {
		inst, err := q.GetInstallationForUpdate(ctx, id)
		if err != nil {
			return err
		}
		pv, pkg, up, err := s.plan(ctx, q, p, inst, req.Version)
		if err != nil {
			return err
		}
		keep, accepted, err := decide(inst, pv, pkg, req)
		if err != nil {
			return err
		}
		made, err := up.Update(ctx, q, inst, Input{Project: inst.ProjectID, By: p, Registry: inst.RegistryID, Package: pkg, Undo: undo.add}, keep)
		if err != nil {
			return err
		}
		state, _ := json.Marshal(installedRecord{Manifest: pkg.Manifest, State: made.State})
		if out, err = q.UpdateInstallation(ctx, db.UpdateInstallationParams{ID: inst.ID, Version: pkg.Item.Version, Checksum: pkg.Item.Checksum,
			AcceptedPermissions: accepted, Installed: state}); err != nil {
			return err
		}
		return emit(ctx, q, "marketplace.updated", out)
	})
	return out, err
}

// decide holds an update to what the person said: the checksum they previewed, every permission
// it adds accepted, an older version only when they asked for one, and their own changes settled.
// It answers the parts to keep and the permissions held after it.
func decide(inst db.Installation, pv Preview, pkg Package, req UpdateRequest) (map[string]bool, []byte, error) {
	if pkg.Item.Checksum != req.Checksum {
		return nil, nil, fmt.Errorf("%w: %s %s is not the package that was accepted (its checksum differs)", ErrConflict, inst.Name, req.Version)
	}
	if pv.From == pv.To && pkg.Item.Checksum == inst.Checksum {
		return nil, nil, fmt.Errorf("%w: %s %s is installed already", ErrConflict, inst.Name, pv.To)
	}
	if CompareVersions(pv.To, pv.From) < 0 && !req.Downgrade {
		return nil, nil, invalid("%s is older than the %s installed; say downgrade to go back to it", pv.To, pv.From)
	}
	if missing := NewPermissions(pv.Added, req.Accepted); len(missing) > 0 {
		return nil, nil, &PermissionsError{Missing: missing}
	}
	keep := map[string]bool{}
	switch req.Resolve {
	case "":
		if len(pv.Local) > 0 {
			return nil, nil, &LocalChangesError{Changes: pv.Local}
		}
	case KeepLocal:
		for _, c := range pv.Local {
			keep[c.Part] = true
		}
	case TakeTheirs:
	default:
		return nil, nil, invalid("resolve is %s or %s", KeepLocal, TakeTheirs)
	}
	var old []string
	_ = json.Unmarshal(inst.AcceptedPermissions, &old)
	held := map[string]bool{}
	for _, a := range normalPermissions(append(old, req.Accepted...)) {
		held[a] = true
	}
	var accepted []string
	for _, asked := range pkg.Manifest.Permissions {
		if held[asked] {
			accepted = append(accepted, asked)
		}
	}
	raw, _ := json.Marshal(orNone(accepted))
	return keep, raw, nil
}

// same says whether two versions of a part are the same, JSON by its meaning and text by its words.
func same(a, b string) bool {
	if a == b {
		return true
	}
	var x, y any
	if json.Unmarshal([]byte(a), &x) == nil && json.Unmarshal([]byte(b), &y) == nil {
		xa, _ := json.Marshal(x)
		yb, _ := json.Marshal(y)
		return string(xa) == string(yb)
	}
	return strings.TrimSpace(a) == strings.TrimSpace(b)
}

var (
	_ Updater = AgentInstaller{}
	_ Updater = SkillInstaller{}
	_ Updater = IntegrationInstaller{}
	_ Updater = ProcessInstaller{}
)

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
