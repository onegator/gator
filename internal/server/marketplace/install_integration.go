package marketplace

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/onegator/gator/internal/server/store/db"
)

// Track installs one track of integration: a remote MCP server, or a native plugin. A new track
// is a new type in the list the integration installer is built with.
type Track interface {
	Track() string
	Scopes() Scopes
	Install(ctx context.Context, tx *db.Queries, in Input, im IntegrationManifest) (Installed, error)
	Remove(ctx context.Context, tx *db.Queries, inst db.Installation) error
}

// IntegrationInstaller installs integration packages by their track.
type IntegrationInstaller struct {
	Integration
	tracks map[string]Track
}

// NewIntegrationInstaller builds it with the tracks it installs.
func NewIntegrationInstaller(tracks ...Track) IntegrationInstaller {
	ii := IntegrationInstaller{tracks: map[string]Track{}}
	for _, t := range tracks {
		ii.tracks[t.Track()] = t
	}
	return ii
}

// Scopes implements Installer: what any of its tracks installs into; each track holds its own.
func (ii IntegrationInstaller) Scopes() Scopes {
	var s Scopes
	for _, t := range ii.tracks {
		s.Project = s.Project || t.Scopes().Project
		s.Workspace = s.Workspace || t.Scopes().Workspace
	}
	return s
}

// Install implements Installer.
func (ii IntegrationInstaller) Install(ctx context.Context, tx *db.Queries, in Input) (Installed, error) {
	im, err := ParseIntegration(in.Package.Files)
	if err != nil {
		return Installed{}, err
	}
	t, ok := ii.tracks[im.Kind]
	if !ok {
		return Installed{}, invalid("this server does not install %s integrations", im.Kind)
	}
	if sc := t.Scopes(); (in.Project.Valid && !sc.Project) || (!in.Project.Valid && !sc.Workspace) {
		if in.Project.Valid {
			return Installed{}, invalid("a %s integration is installed for the whole workspace, by its admins", im.Kind)
		}
		return Installed{}, invalid("a %s integration is installed into a project", im.Kind)
	}
	return t.Install(ctx, tx, in, im)
}

// Remove implements Installer, by the track the installation was made with.
func (ii IntegrationInstaller) Remove(ctx context.Context, tx *db.Queries, inst db.Installation) error {
	im, err := installedIntegration(inst)
	if err != nil {
		return err
	}
	t, ok := ii.tracks[im.Kind]
	if !ok {
		return invalid("this server does not install %s integrations", im.Kind)
	}
	return t.Remove(ctx, tx, inst)
}

// installedIntegration is the integration manifest an installation was made from.
func installedIntegration(inst db.Installation) (IntegrationManifest, error) {
	var rec installedRecord
	var im IntegrationManifest
	if err := json.Unmarshal(inst.Installed, &rec); err != nil {
		return im, err
	}
	err := json.Unmarshal(rec.Manifest.Detail, &im)
	return im, err
}

// MCPTrack installs a remote MCP server for a project (or every project): its address, how it
// signs in and the tools agents may call from the start. Its credentials come afterwards, from
// a person, and never leave the server.
type MCPTrack struct{}

// Track implements Track.
func (MCPTrack) Track() string { return IntegrationMCP }

// Scopes implements Track.
func (MCPTrack) Scopes() Scopes { return Scopes{Project: true, Workspace: true} }

// Install implements Track.
func (MCPTrack) Install(ctx context.Context, tx *db.Queries, in Input, im IntegrationManifest) (Installed, error) {
	oauth := []byte("{}")
	if im.OAuth != nil {
		oauth, _ = json.Marshal(im.OAuth)
	}
	source, _ := json.Marshal(map[string]string{"kind": "marketplace", "registry": uuidString(in.Registry), "package": im.Name, "version": im.Version})
	srv, err := tx.CreateMCPServer(ctx, db.CreateMCPServerParams{ProjectID: in.Project, Name: im.Name, DisplayName: im.DisplayName,
		Url: im.URL, Auth: im.Auth, AllowedTools: orNone(im.ToolsDefaultAllow), Oauth: oauth, Source: source})
	if isUnique(err) {
		return Installed{}, fmt.Errorf("%w: there is an MCP server named %s here already", ErrConflict, im.Name)
	}
	if err != nil {
		return Installed{}, err
	}
	return Installed{Target: srv.ID}, nil
}

// Remove implements Track: the server goes, and its credentials with it.
func (MCPTrack) Remove(ctx context.Context, tx *db.Queries, inst db.Installation) error {
	if inst.TargetID.Valid {
		_, err := tx.DeleteMCPServer(ctx, inst.TargetID)
		return err
	}
	return nil
}
