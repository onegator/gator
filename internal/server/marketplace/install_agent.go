package marketplace

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/onegator/gator/internal/server/agents"
	"github.com/onegator/gator/internal/server/store/db"
)

// AgentInstaller installs agent packages: a member of the project (or the workspace) that people
// can mention the moment it is installed.
type AgentInstaller struct {
	Agent
}

// Scopes implements Installer.
func (AgentInstaller) Scopes() Scopes { return Scopes{Project: true, Workspace: true} }

// stateInstructions is the part of an installed agent a person may edit afterwards.
const stateInstructions = "instructions"

// Install implements Installer. The agent's handle is the package's suggestion unless the person
// chose another; one already taken where people would see it is refused, and they choose again.
func (AgentInstaller) Install(ctx context.Context, tx *db.Queries, in Input) (Installed, error) {
	am, instructions, err := ParseAgent(in.Package.Files)
	if err != nil {
		return Installed{}, err
	}
	handle := am.Handle
	if h := strings.TrimPrefix(strings.TrimSpace(in.Handle), "@"); h != "" {
		handle = h
	}
	if err := agents.Check(handle, am.DisplayName, am.Description, am.Tier); err != nil {
		return Installed{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var roleKey *string
	if am.RoleKey != "" {
		roleKey = &am.RoleKey
	}
	policy, _ := json.Marshal(am.Policy)
	tools, _ := json.Marshal(agentTools(am))
	skills, _ := json.Marshal(orNone(am.Skills))
	source, _ := json.Marshal(map[string]string{"kind": agents.SourceMarketplace, "registry": uuidString(in.Registry),
		"package": am.Name, "version": am.Version})
	avatar := am.Avatar
	a, err := tx.CreateInstalledAgent(ctx, db.CreateInstalledAgentParams{ProjectID: in.Project, Handle: handle, DisplayName: am.DisplayName,
		Description: am.Description, Avatar: avatar, RoleKey: roleKey, Instructions: instructions, Tier: am.Tier, Policy: policy,
		Tools: tools, Skills: skills, Source: source, CreatedBy: in.By.UserID})
	if isUnique(err) {
		return Installed{}, fmt.Errorf("%w: @%s is taken where people would mention it; choose another handle", ErrConflict, handle)
	}
	if err != nil {
		return Installed{}, err
	}
	return Installed{Target: a.ID, State: map[string]string{stateInstructions: instructions}}, nil
}

// Remove implements Installer: the agent is put away, its turns kept as the record of what it did.
// One still answering is left until it is done.
func (AgentInstaller) Remove(ctx context.Context, tx *db.Queries, inst db.Installation) error {
	if !inst.TargetID.Valid {
		return nil
	}
	active, err := tx.CountActiveTurnsOfAgent(ctx, inst.TargetID)
	if err != nil {
		return err
	}
	if active > 0 {
		return fmt.Errorf("%w: the agent is still answering; stop its turns or wait for them first", ErrConflict)
	}
	_, err = tx.DeleteAgent(ctx, inst.TargetID)
	return err
}

// agentTools are the MCP servers (or server/tool) an agent may call: those it names and those it
// needs.
func agentTools(am AgentManifest) []string {
	out := slices.Clone(am.Tools)
	for _, s := range am.Requires.MCP {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return orNone(out)
}

func orNone(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
