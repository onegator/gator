package marketplace

import (
	"context"
	"encoding/json"

	"github.com/onegator/gator/internal/server/store/db"
)

// KnowledgeInstaller installs knowledge packs as knowledge_packs has always held them: for the
// workspace, into the library every project can switch on; for a project, switched on there too,
// in place of any other version of the pack.
type KnowledgeInstaller struct {
	Knowledge
}

// Scopes implements Installer.
func (KnowledgeInstaller) Scopes() Scopes { return Scopes{Project: true, Workspace: true} }

// Install implements Installer.
func (KnowledgeInstaller) Install(ctx context.Context, tx *db.Queries, in Input) (Installed, error) {
	pack, err := ParsePack(in.Package.Files)
	if err != nil {
		return Installed{}, err
	}
	manifest, _ := json.Marshal(pack.Manifest)
	files, _ := json.Marshal(pack.Files)
	stored, err := tx.UpsertKnowledgePack(ctx, db.UpsertKnowledgePackParams{Name: pack.Manifest.Name, Version: pack.Manifest.Version,
		Source: "git", Url: "marketplace:" + uuidString(in.Registry), Checksum: pack.Checksum(), Manifest: manifest, Files: files})
	if err != nil {
		return Installed{}, err
	}
	if in.Project.Valid {
		if _, err := tx.DisablePacksByName(ctx, db.DisablePacksByNameParams{ProjectID: in.Project, Name: pack.Manifest.Name}); err != nil {
			return Installed{}, err
		}
		if err := tx.EnablePackForProject(ctx, db.EnablePackForProjectParams{ProjectID: in.Project, PackID: stored.ID, Position: 100}); err != nil {
			return Installed{}, err
		}
	}
	return Installed{Target: stored.ID}, nil
}

// Remove implements Installer: a project's pack is switched off there; the workspace's stays in
// the library, where projects that switched it on keep it.
func (KnowledgeInstaller) Remove(ctx context.Context, tx *db.Queries, inst db.Installation) error {
	if inst.ProjectID.Valid && inst.TargetID.Valid {
		_, err := tx.DisablePackForProject(ctx, db.DisablePackForProjectParams{ProjectID: inst.ProjectID, PackID: inst.TargetID})
		return err
	}
	return nil
}

// Current implements Updater: a pack is not edited in place.
func (KnowledgeInstaller) Current(context.Context, *db.Queries, db.Installation) (map[string]string, error) {
	return map[string]string{}, nil
}

// Incoming implements Updater.
func (KnowledgeInstaller) Incoming(Package) (map[string]string, error) {
	return map[string]string{}, nil
}

// Update implements Updater: the new version, switched on in place of the old one.
func (k KnowledgeInstaller) Update(ctx context.Context, tx *db.Queries, _ db.Installation, in Input, _ map[string]bool) (Installed, error) {
	return k.Install(ctx, tx, in)
}

var _ Updater = KnowledgeInstaller{}
