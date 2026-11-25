package marketplace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// Installer puts one kind of package into a project, or the workspace, and takes it out again.
// Each kind is a type of its own; the service holds a list of them and asks the one whose kind a
// package is. Install and Remove run in the transaction that writes the installation, so either
// both happen or neither does.
type Installer interface {
	Kind
	// Scopes says where the kind installs: a project, the workspace, or both.
	Scopes() Scopes
	// Install makes what the package installs and answers its id, and what of it a person may
	// change afterwards (to tell an update from a local edit).
	Install(ctx context.Context, tx *db.Queries, in Input) (Installed, error)
	// Remove takes out what Install made.
	Remove(ctx context.Context, tx *db.Queries, inst db.Installation) error
}

// Scopes are where a kind installs.
type Scopes struct{ Project, Workspace bool }

// Input is one installation's: where, by whom, what, with what a person chose.
type Input struct {
	Project  pgtype.UUID // zero: the workspace
	By       auth.Principal
	Registry pgtype.UUID
	Package  Package
	// Handle replaces an agent's suggested handle, when it is taken.
	Handle string
}

// Installed is what an installer made.
type Installed struct {
	Target pgtype.UUID
	// State is what a person may change afterwards, as installed: an update compares it with
	// what is there now.
	State map[string]string
}

// Request asks for a package to be installed.
type Request struct {
	Project  pgtype.UUID // zero: the workspace
	Kind     string
	Name     string
	Version  string      // empty: the newest listed
	Registry pgtype.UUID // zero: whichever registry offers it, Gator's first
	// Accepted are the permissions the person accepted, line by line. Every permission the
	// package asks for must be among them.
	Accepted []string
	Handle   string
}

// PermissionsError is an install or update that asks for permissions nobody accepted.
type PermissionsError struct{ Missing []string }

func (e *PermissionsError) Error() string {
	return fmt.Sprintf("%s: accept %s", ErrPermissions, strings.Join(e.Missing, "; "))
}

// Unwrap lets errors.Is find ErrPermissions.
func (e *PermissionsError) Unwrap() error { return ErrPermissions }

// installStore reads installations. *db.Queries is one.
type installStore interface {
	GetInstallation(ctx context.Context, id pgtype.UUID) (db.Installation, error)
	ListInstallations(ctx context.Context, projectID pgtype.UUID) ([]db.Installation, error)
	FindInstallation(ctx context.Context, arg db.FindInstallationParams) (db.Installation, error)
	ListSkillNamesVisibleIn(ctx context.Context, projectID pgtype.UUID) ([]string, error)
}

// packages is where installing takes a package from. *Catalog is one.
type packages interface {
	Versions(ctx context.Context, p auth.Principal, kind, name string, registry pgtype.UUID) ([]Version, error)
	Package(ctx context.Context, registry pgtype.UUID, kind, name, version string) (Package, error)
}

// roles answers a person's role in a project. auth.Authorizer is one.
type roles interface {
	ProjectRole(ctx context.Context, p auth.Principal, project pgtype.UUID) (auth.Role, error)
}

// capabilities answers what a project's plugins can do. plugins.Capabilities is one.
type capabilities interface {
	Capabilities(ctx context.Context, projectID pgtype.UUID) ([]string, error)
}

// Tx runs fn in one transaction. store.InTx over a pool is one.
type Tx func(ctx context.Context, fn func(q *db.Queries) error) error

// Installations installs packages and keeps the record of what was installed, with what
// accepted.
type Installations struct {
	store      installStore
	packages   packages
	roles      roles
	caps       capabilities
	tx         Tx
	installers map[string]Installer
}

// NewInstallations builds it with the installers of every kind it installs.
func NewInstallations(store installStore, pkgs packages, rs roles, caps capabilities, tx Tx, installers ...Installer) *Installations {
	s := &Installations{store: store, packages: pkgs, roles: rs, caps: caps, tx: tx, installers: map[string]Installer{}}
	for _, in := range installers {
		s.installers[in.Kind()] = in
	}
	return s
}

// Install installs a package where the request says, once a person accepted every permission
// it asks for. A project's admin installs into the project, a workspace admin into the workspace.
func (s *Installations) Install(ctx context.Context, p auth.Principal, req Request) (db.Installation, error) {
	if err := s.canManage(ctx, p, req.Project); err != nil {
		return db.Installation{}, err
	}
	installer, ok := s.installers[req.Kind]
	if !ok {
		return db.Installation{}, invalid("this server does not install %ss", req.Kind)
	}
	if sc := installer.Scopes(); (req.Project.Valid && !sc.Project) || (!req.Project.Valid && !sc.Workspace) {
		where := "a project"
		if !req.Project.Valid {
			where = "the workspace"
		}
		return db.Installation{}, invalid("a %s does not install into %s", req.Kind, where)
	}
	v, err := s.choose(ctx, p, req.Kind, req.Name, req.Version, req.Registry)
	if err != nil {
		return db.Installation{}, err
	}
	pkg, err := s.packages.Package(ctx, v.RegistryID, req.Kind, req.Name, v.Manifest.Version)
	if err != nil {
		return db.Installation{}, err
	}
	if missing := NewPermissions(pkg.Manifest.Permissions, req.Accepted); len(missing) > 0 {
		return db.Installation{}, &PermissionsError{Missing: missing}
	}
	accepted, _ := json.Marshal(normalPermissions(req.Accepted))
	var inst db.Installation
	err = s.tx(ctx, func(q *db.Queries) error {
		made, err := installer.Install(ctx, q, Input{Project: req.Project, By: p, Registry: v.RegistryID, Package: pkg, Handle: req.Handle})
		if err != nil {
			return err
		}
		state, _ := json.Marshal(installedRecord{Manifest: pkg.Manifest, State: made.State})
		inst, err = q.InsertInstallation(ctx, db.InsertInstallationParams{ProjectID: req.Project, RegistryID: v.RegistryID, Kind: req.Kind,
			Name: req.Name, Version: pkg.Item.Version, Checksum: pkg.Item.Checksum, TargetID: made.Target, AcceptedPermissions: accepted,
			Installed: state, InstalledBy: p.UserID})
		if isUnique(err) {
			return fmt.Errorf("%w: %s %s is installed here already; update it instead", ErrConflict, req.Kind, req.Name)
		}
		if err != nil {
			return err
		}
		return emit(ctx, q, "marketplace.installed", inst)
	})
	return inst, err
}

// installedRecord is installations.installed.
type installedRecord struct {
	Manifest Manifest          `json:"manifest"`
	State    map[string]string `json:"state,omitempty"`
}

// choose finds the version to install: the one asked for, or the newest listed.
func (s *Installations) choose(ctx context.Context, p auth.Principal, kind, name, version string, registry pgtype.UUID) (Version, error) {
	vs, err := s.packages.Versions(ctx, p, kind, name, registry)
	if err != nil {
		return Version{}, err
	}
	for _, v := range vs {
		if (version != "" && v.Manifest.Version == version) || (version == "" && v.Listed) {
			return v, nil
		}
	}
	return Version{}, ErrNotFound
}

// Get answers an installation the caller may see.
func (s *Installations) Get(ctx context.Context, p auth.Principal, id pgtype.UUID) (db.Installation, error) {
	inst, err := s.store.GetInstallation(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return inst, ErrNotFound
	}
	if err != nil {
		return inst, err
	}
	if err := s.canSee(ctx, p, inst.ProjectID); err != nil {
		return inst, err
	}
	return inst, nil
}

// List answers a project's installations (or the workspace's, project zero).
func (s *Installations) List(ctx context.Context, p auth.Principal, project pgtype.UUID) ([]db.Installation, error) {
	if err := s.canSee(ctx, p, project); err != nil {
		return nil, err
	}
	return s.store.ListInstallations(ctx, project)
}

// Remove uninstalls: what the installation made goes, then the installation.
func (s *Installations) Remove(ctx context.Context, p auth.Principal, id pgtype.UUID) error {
	inst, err := s.Get(ctx, p, id)
	if err != nil {
		return err
	}
	if err := s.canManage(ctx, p, inst.ProjectID); err != nil {
		return err
	}
	installer, ok := s.installers[inst.Kind]
	if !ok {
		return invalid("this server does not install %ss", inst.Kind)
	}
	return s.tx(ctx, func(q *db.Queries) error {
		if err := installer.Remove(ctx, q, inst); err != nil {
			return err
		}
		if _, err := q.DeleteInstallation(ctx, inst.ID); err != nil {
			return err
		}
		return emit(ctx, q, "marketplace.removed", inst)
	})
}

// State is what a project has of a package and what it lacks to run it.
type State struct {
	Installed *InstalledState
	Missing   []string
}

// InstalledState is an installation as a listing shows it.
type InstalledState struct {
	ID              pgtype.UUID
	Version         string
	UpdateAvailable bool
	Workspace       bool
}

// State says what project has of the package m describes, whether a newer version waits, and
// what the project lacks that the package needs.
func (s *Installations) State(ctx context.Context, p auth.Principal, project pgtype.UUID, m Manifest) (State, error) {
	var st State
	inst, err := s.store.FindInstallation(ctx, db.FindInstallationParams{Kind: m.Kind, Name: m.Name, ProjectID: project})
	switch {
	case err == nil:
		_, newer := s.Newer(ctx, p, inst)
		st.Installed = &InstalledState{ID: inst.ID, Version: inst.Version, Workspace: !inst.ProjectID.Valid, UpdateAvailable: newer}
	case !errors.Is(err, pgx.ErrNoRows):
		return st, err
	}
	st.Missing, err = s.missing(ctx, project, m.Requires)
	return st, err
}

// Newer answers the newest version the installation's registry offers, when it is newer than the
// one installed: the "Update" chip. Nothing updates by itself.
func (s *Installations) Newer(ctx context.Context, p auth.Principal, inst db.Installation) (string, bool) {
	newest, err := s.choose(ctx, p, inst.Kind, inst.Name, "", inst.RegistryID)
	if err != nil || CompareVersions(newest.Manifest.Version, inst.Version) <= 0 {
		return "", false
	}
	return newest.Manifest.Version, true
}

// missing is what of r the project does not have.
func (s *Installations) missing(ctx context.Context, project pgtype.UUID, r Requires) ([]string, error) {
	var out []string
	if len(r.Capabilities) > 0 && s.caps != nil {
		have, err := s.caps.Capabilities(ctx, project)
		if err != nil {
			return nil, err
		}
		for _, c := range r.Capabilities {
			if !slices.Contains(have, c) {
				out = append(out, "capability "+c)
			}
		}
	}
	if len(r.Skills) > 0 {
		have, err := s.store.ListSkillNamesVisibleIn(ctx, project)
		if err != nil {
			return nil, err
		}
		for _, sk := range r.Skills {
			if !slices.Contains(have, sk) {
				out = append(out, "skill "+sk)
			}
		}
	}
	return out, nil
}

func (s *Installations) canSee(ctx context.Context, p auth.Principal, project pgtype.UUID) error {
	if p.Kind != auth.KindUser {
		return ErrForbidden
	}
	if !project.Valid {
		return nil
	}
	role, err := s.roles.ProjectRole(ctx, p, project)
	if err != nil {
		return err
	}
	if role < auth.RoleViewer {
		return ErrNotFound
	}
	return nil
}

func (s *Installations) canManage(ctx context.Context, p auth.Principal, project pgtype.UUID) error {
	if p.Kind != auth.KindUser {
		return ErrForbidden
	}
	if !project.Valid {
		if p.IsWorkspaceAdmin() {
			return nil
		}
		return ErrForbidden
	}
	role, err := s.roles.ProjectRole(ctx, p, project)
	if err != nil {
		return err
	}
	switch {
	case role < auth.RoleViewer:
		return ErrNotFound
	case role < auth.RoleAdmin:
		return ErrForbidden
	}
	return nil
}

// emit records what happened to an installation in the outbox: the project's screens, or every
// screen for the workspace's, refresh their agents, skills and integrations.
func emit(ctx context.Context, q *db.Queries, typ string, inst db.Installation) error {
	payload, _ := json.Marshal(map[string]any{"installation": uuidString(inst.ID), "kind": inst.Kind, "name": inst.Name,
		"version": inst.Version, "target": uuidString(inst.TargetID)})
	aggregate, id := "project", inst.ProjectID
	if !id.Valid {
		aggregate, id = "installation", inst.ID
	}
	_, err := q.InsertEvent(ctx, db.InsertEventParams{Type: typ, Aggregate: aggregate, AggregateID: id, Payload: payload})
	return err
}

func isUnique(err error) bool {
	pe, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pe.Code == "23505"
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	v, _ := u.Value()
	s, _ := v.(string)
	return s
}
