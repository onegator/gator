package marketplace

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// SkillInstaller installs skill packages: a folder in the Agent Skills format, kept whole, that
// agents of the project (or of every project) carry by name. Nothing of it runs when it is
// installed; its scripts are the agent's to run, in a job, with the job's rights.
type SkillInstaller struct {
	Skill
}

// Scopes implements Installer.
func (SkillInstaller) Scopes() Scopes { return Scopes{Project: true, Workspace: true} }

// stateSkill is the part of an installed skill a person may edit afterwards.
const stateSkill = SkillFile

// Install implements Installer. A skill installed on its own takes the place of one an agent
// brought with it.
func (s SkillInstaller) Install(ctx context.Context, tx *db.Queries, in Input) (Installed, error) {
	source := map[string]string{"kind": "marketplace", "registry": uuidString(in.Registry), "package": in.Package.Item.Name,
		"version": in.Package.Item.Version}
	sk, err := s.put(ctx, tx, in.Project, in.Package.Files, source)
	if err != nil {
		return Installed{}, err
	}
	md, _ := in.Package.Files.Get(SkillFile)
	return Installed{Target: sk.ID, State: map[string]string{stateSkill: string(md.Data)}}, nil
}

// Remove implements Installer.
func (SkillInstaller) Remove(ctx context.Context, tx *db.Queries, inst db.Installation) error {
	if inst.TargetID.Valid {
		_, err := tx.DeleteSkill(ctx, inst.TargetID)
		return err
	}
	return nil
}

// PutBundled keeps a skill an agent package brings, in the agent's scope. A skill of that name
// installed on its own, or brought by another agent, stays as it is: the agent carries it by name.
func (s SkillInstaller) PutBundled(ctx context.Context, tx *db.Queries, project pgtype.UUID, agent string, in Input, files Files) error {
	md, ok := files.Get(SkillFile)
	if !ok {
		return invalid("%s is missing", SkillFile)
	}
	sm, _, err := ParseSkill(md.Data)
	if err != nil {
		return err
	}
	cur, err := tx.GetSkillIn(ctx, db.GetSkillInParams{ProjectID: project, Name: sm.Name})
	switch {
	case err == nil:
		var src map[string]string
		_ = json.Unmarshal(cur.Source, &src)
		if src["agent"] != agent {
			return nil
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	source := map[string]string{"kind": "marketplace", "registry": uuidString(in.Registry), "package": in.Package.Item.Name,
		"version": in.Package.Item.Version, "agent": agent}
	_, err = s.put(ctx, tx, project, files, source)
	return err
}

// RemoveBundled takes away the skills an agent package brought, once no agent of the scope
// carries them.
func (SkillInstaller) RemoveBundled(ctx context.Context, tx *db.Queries, project pgtype.UUID, agent string) error {
	return tx.DeleteBundledSkills(ctx, db.DeleteBundledSkillsParams{ProjectID: project, Agent: agent})
}

func (SkillInstaller) put(ctx context.Context, tx *db.Queries, project pgtype.UUID, files Files, source map[string]string) (db.Skill, error) {
	m, err := describeSkill(files)
	if err != nil {
		return db.Skill{}, err
	}
	version := m.Version
	if version == "" {
		version = source["version"]
	}
	data, _ := json.Marshal(files)
	src, _ := json.Marshal(source)
	return tx.UpsertSkill(ctx, db.UpsertSkillParams{ProjectID: project, Name: m.Name, Version: version, Description: m.Description,
		Files: data, Source: src})
}

// Current implements Updater: a skill's SKILL.md, as it is now.
func (SkillInstaller) Current(ctx context.Context, tx *db.Queries, inst db.Installation) (map[string]string, error) {
	sk, err := tx.GetSkill(ctx, inst.TargetID)
	if err != nil {
		return nil, err
	}
	var files Files
	_ = json.Unmarshal(sk.Files, &files)
	md, _ := files.Get(SkillFile)
	return map[string]string{stateSkill: string(md.Data)}, nil
}

// Incoming implements Updater.
func (SkillInstaller) Incoming(pkg Package) (map[string]string, error) {
	md, _ := pkg.Files.Get(SkillFile)
	return map[string]string{stateSkill: string(md.Data)}, nil
}

// Update implements Updater: the new version's folder, with the SKILL.md a person kept.
func (s SkillInstaller) Update(ctx context.Context, tx *db.Queries, inst db.Installation, in Input, keep map[string]bool) (Installed, error) {
	files := in.Package.Files
	incoming, _ := files.Get(SkillFile)
	if keep[stateSkill] {
		cur, err := s.Current(ctx, tx, inst)
		if err != nil {
			return Installed{}, err
		}
		kept := make(Files, 0, len(files))
		for _, f := range files {
			if f.Path == SkillFile {
				f = File{Path: SkillFile, Data: []byte(cur[stateSkill])}
			}
			kept = append(kept, f)
		}
		files = kept
	}
	source := map[string]string{"kind": "marketplace", "registry": uuidString(in.Registry), "package": in.Package.Item.Name,
		"version": in.Package.Item.Version}
	sk, err := s.put(ctx, tx, inst.ProjectID, files, source)
	if err != nil {
		return Installed{}, err
	}
	return Installed{Target: sk.ID, State: map[string]string{stateSkill: string(incoming.Data)}}, nil
}
