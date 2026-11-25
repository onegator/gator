package marketplace

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// ProcessInstaller installs process packages: the template of one kind of task, and the prompts
// of the roles it brings, into a project's process_config (or the workspace's, for every
// project). What it replaces is kept with the installation and put back when it is removed.
type ProcessInstaller struct {
	Process
}

// Scopes implements Installer.
func (ProcessInstaller) Scopes() Scopes { return Scopes{Project: true, Workspace: true} }

// What of the config an installation replaced, kept in its state.
const (
	stateTemplate = "previous_template"
	stateRoles    = "previous_roles"
	stateKind     = "kind"
)

// Install implements Installer.
func (ProcessInstaller) Install(ctx context.Context, tx *db.Queries, in Input) (Installed, error) {
	pp, err := ParseProcess(in.Package.Files)
	if err != nil {
		return Installed{}, err
	}
	cfg, err := readConfig(ctx, tx, in.Project)
	if err != nil {
		return Installed{}, err
	}
	templates, roles := cfg.object("templates"), cfg.object("roles")
	kind := pp.Template.Kind
	prevRoles := map[string]json.RawMessage{}
	for role := range pp.Roles {
		if prev, ok := roles[role]; ok {
			prevRoles[role] = prev
		}
	}
	state := map[string]string{stateKind: kind, stateTemplate: string(templates[kind])}
	pr, _ := json.Marshal(prevRoles)
	state[stateRoles] = string(pr)

	t, _ := json.Marshal(pp.Template)
	templates[kind] = t
	for role, prompt := range pp.Roles {
		roles[role], _ = json.Marshal(prompt)
	}
	cfg.set("templates", templates)
	cfg.set("roles", roles)
	if err := writeConfig(ctx, tx, in.Project, cfg); err != nil {
		return Installed{}, err
	}
	return Installed{State: state}, nil
}

// Remove implements Installer: the template and prompts it set go, and what they replaced comes
// back.
func (ProcessInstaller) Remove(ctx context.Context, tx *db.Queries, inst db.Installation) error {
	var rec installedRecord
	if err := json.Unmarshal(inst.Installed, &rec); err != nil {
		return err
	}
	pp, err := processOf(rec)
	if err != nil {
		return err
	}
	cfg, err := readConfig(ctx, tx, inst.ProjectID)
	if err != nil {
		return err
	}
	templates, roles := cfg.object("templates"), cfg.object("roles")
	kind := rec.State[stateKind]
	if prev := rec.State[stateTemplate]; prev != "" {
		templates[kind] = json.RawMessage(prev)
	} else {
		delete(templates, kind)
	}
	var prevRoles map[string]json.RawMessage
	_ = json.Unmarshal([]byte(rec.State[stateRoles]), &prevRoles)
	for role := range pp.Roles {
		if prev, ok := prevRoles[role]; ok {
			roles[role] = prev
		} else {
			delete(roles, role)
		}
	}
	cfg.set("templates", templates)
	cfg.set("roles", roles)
	return writeConfig(ctx, tx, inst.ProjectID, cfg)
}

// processOf is the process manifest an installation was made from; its roles are what it set.
func processOf(rec installedRecord) (ProcessManifest, error) {
	var pm ProcessManifest
	err := json.Unmarshal(rec.Manifest.Detail, &pm)
	return pm, err
}

// config is a process_config document read as its top-level keys, so what this installer does
// not touch (policy, quality, autopilot …) is written back as it was.
type config map[string]json.RawMessage

func (c config) object(key string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	_ = json.Unmarshal(c[key], &out)
	return out
}

func (c config) set(key string, v map[string]json.RawMessage) {
	if len(v) == 0 {
		delete(c, key)
		return
	}
	c[key], _ = json.Marshal(v)
}

func readConfig(ctx context.Context, tx *db.Queries, project pgtype.UUID) (config, error) {
	var raw []byte
	if project.Valid {
		b, err := tx.LockProjectConfig(ctx, project)
		if err != nil {
			return nil, err
		}
		raw = b
	} else {
		ws, err := tx.GetWorkspaceSettings(ctx)
		if err != nil {
			return nil, err
		}
		raw = ws.ProcessConfig
	}
	cfg := config{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// writeConfig writes a config back once the process would read it: a template the process
// refuses is refused here, not when a task tries to use it.
func writeConfig(ctx context.Context, tx *db.Queries, project pgtype.UUID, cfg config) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	parsed, err := process.ParseProjectConfig(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	for kind, t := range parsed.Templates {
		if err := t.Validate(); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInvalid, kind, err)
		}
	}
	if project.Valid {
		_, err = tx.UpdateProjectConfig(ctx, db.UpdateProjectConfigParams{ID: project, ProcessConfig: raw})
		return err
	}
	_, err = tx.SetWorkspaceProcessConfig(ctx, raw)
	return err
}
