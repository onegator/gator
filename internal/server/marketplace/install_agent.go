package marketplace

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/agents"
	"github.com/onegator/gator/internal/server/store/db"
)

// AgentInstaller installs agent packages: a member of the project (or the workspace) that people
// can mention the moment it is installed, with the skills it brings.
type AgentInstaller struct {
	Agent
	skills bundledSkills
}

// bundledSkills keeps the skills an agent package brings. *SkillInstaller is one.
type bundledSkills interface {
	PutBundled(ctx context.Context, tx *db.Queries, project pgtype.UUID, agent string, in Input, files Files) error
	RemoveBundled(ctx context.Context, tx *db.Queries, project pgtype.UUID, agent string) error
}

// NewAgentInstaller builds it.
func NewAgentInstaller(skills bundledSkills) AgentInstaller { return AgentInstaller{skills: skills} }

// Scopes implements Installer.
func (AgentInstaller) Scopes() Scopes { return Scopes{Project: true, Workspace: true} }

// stateInstructions is the part of an installed agent a person may edit afterwards.
const stateInstructions = "instructions"

// Install implements Installer. The agent's handle is the package's suggestion unless the person
// chose another; one already taken where people would see it is refused, and they choose again.
func (ai AgentInstaller) Install(ctx context.Context, tx *db.Queries, in Input) (Installed, error) {
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
	for _, name := range am.Skills {
		if files := in.Package.Files.Under("skills/" + name); len(files) > 0 {
			if err := ai.skills.PutBundled(ctx, tx, in.Project, am.Name, in, files); err != nil {
				return Installed{}, err
			}
		}
	}
	return Installed{Target: a.ID, State: partsOf(am, instructions)}, nil
}

// Remove implements Installer: the agent is put away, its turns kept as the record of what it did.
// One still answering is left until it is done.
func (ai AgentInstaller) Remove(ctx context.Context, tx *db.Queries, inst db.Installation) error {
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
	if _, err := tx.DeleteAgent(ctx, inst.TargetID); err != nil {
		return err
	}
	return ai.skills.RemoveBundled(ctx, tx, inst.ProjectID, inst.Name)
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

// The parts of an installed agent a person may change afterwards, each compared with what was
// installed before an update touches it.
const (
	partTools  = "tools"
	partSkills = "skills"
	partPolicy = "policy"
	partTier   = "tier"
	partRole   = "role_key"
)

// partsOf are an agent package's parts, as an agent row keeps them.
func partsOf(am AgentManifest, instructions string) map[string]string {
	tools, _ := json.Marshal(agentTools(am))
	skills, _ := json.Marshal(orNone(am.Skills))
	policy, _ := json.Marshal(am.Policy)
	return map[string]string{stateInstructions: instructions, partTools: string(tools), partSkills: string(skills),
		partPolicy: string(policy), partTier: am.Tier, partRole: am.RoleKey}
}

// partsOfAgent are an agent's parts as they are now.
func partsOfAgent(a db.Agent) map[string]string {
	role := ""
	if a.RoleKey != nil {
		role = *a.RoleKey
	}
	return map[string]string{stateInstructions: a.Instructions, partTools: string(a.Tools), partSkills: string(a.Skills),
		partPolicy: string(a.Policy), partTier: a.Tier, partRole: role}
}

// Current implements Updater.
func (AgentInstaller) Current(ctx context.Context, tx *db.Queries, inst db.Installation) (map[string]string, error) {
	a, err := tx.GetAgent(ctx, inst.TargetID)
	if err != nil {
		return nil, err
	}
	return partsOfAgent(a), nil
}

// Incoming implements Updater.
func (AgentInstaller) Incoming(pkg Package) (map[string]string, error) {
	am, instructions, err := ParseAgent(pkg.Files)
	return partsOf(am, instructions), err
}

// Update implements Updater: the agent takes the new version's name, description and every part
// a person did not ask to keep; it keeps its handle (people know it by it). The skills it brought
// and the new version does not are taken away, unless another agent carries them.
func (ai AgentInstaller) Update(ctx context.Context, tx *db.Queries, inst db.Installation, in Input, keep map[string]bool) (Installed, error) {
	am, instructions, err := ParseAgent(in.Package.Files)
	if err != nil {
		return Installed{}, err
	}
	cur, err := tx.GetAgent(ctx, inst.TargetID)
	if err != nil {
		return Installed{}, err
	}
	incoming, now := partsOf(am, instructions), partsOfAgent(cur)
	next := map[string]string{}
	for part, v := range incoming {
		next[part] = v
		if keep[part] {
			next[part] = now[part]
		}
	}
	if err := agents.Check(cur.Handle, am.DisplayName, am.Description, next[partTier]); err != nil {
		return Installed{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var roleKey *string
	if r := next[partRole]; r != "" {
		roleKey = &r
	}
	source, _ := json.Marshal(map[string]string{"kind": agents.SourceMarketplace, "registry": uuidString(in.Registry),
		"package": am.Name, "version": am.Version})
	if _, err := tx.UpdateInstalledAgent(ctx, db.UpdateInstalledAgentParams{ID: cur.ID, DisplayName: am.DisplayName, Description: am.Description,
		Avatar: am.Avatar, RoleKey: roleKey, Instructions: next[stateInstructions], Tier: next[partTier], Policy: []byte(next[partPolicy]),
		Tools: []byte(next[partTools]), Skills: []byte(next[partSkills]), Source: source}); err != nil {
		return Installed{}, err
	}
	for _, name := range am.Skills {
		if files := in.Package.Files.Under("skills/" + name); len(files) > 0 {
			if err := ai.skills.PutBundled(ctx, tx, inst.ProjectID, am.Name, in, files); err != nil {
				return Installed{}, err
			}
		}
	}
	if err := ai.skills.RemoveBundled(ctx, tx, inst.ProjectID, am.Name); err != nil {
		return Installed{}, err
	}
	return Installed{Target: cur.ID, State: incoming}, nil
}
