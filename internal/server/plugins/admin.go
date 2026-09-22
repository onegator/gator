package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/internal/version"
	"github.com/onegator/gator/plugin"
)

// Install registers a plugin: it starts the command once, reads the manifest from
// initialize, checks it and stores it. Workspace admins only; the command runs as the
// server's user.
func (h *Host) Install(ctx context.Context, name string, command []string) (plugin.Manifest, error) {
	if len(command) == 0 || command[0] == "" {
		return plugin.Manifest{}, fmt.Errorf("%w: command is required", ErrInvalid)
	}
	m, err := h.probe(ctx, command)
	if err != nil {
		return m, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := m.Validate(); err != nil {
		return m, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if m.Name != name {
		return m, fmt.Errorf("%w: the command is plugin %q, not %q", ErrInvalid, m.Name, name)
	}
	if !plugin.CoreSatisfies(version.Version, m.MinCoreVersion) {
		return m, fmt.Errorf("%w: %s needs gator %s or newer (this is %s)", ErrInvalid, name, m.MinCoreVersion, version.Version)
	}
	mb, _ := json.Marshal(m)
	if _, err := db.New(h.pool).UpsertPlugin(ctx, db.UpsertPluginParams{Name: name, Command: command, Version: m.Version, Manifest: mb}); err != nil {
		return m, err
	}
	return m, h.Sync(ctx)
}

// probe reads a plugin's manifest without a project: no config, no secrets.
func (h *Host) probe(ctx context.Context, command []string) (plugin.Manifest, error) {
	var m plugin.Manifest
	env := []string{"PATH=" + envPath(), "LANG=C.UTF-8"}
	cmd, conn, waited, _, err := spawn(command, env, &logWriter{log: h.log.With("plugin_probe", command[0])}, nil)
	if err != nil {
		return m, err
	}
	defer func() {
		kill(cmd)
		<-waited
	}()
	cctx, cancel := context.WithTimeout(ctx, min(h.cfg.CallTimeout, 10*time.Second))
	defer cancel()
	err = conn.Call(cctx, plugin.MethodInitialize, plugin.InitializeParams{CoreVersion: version.Version, ProtocolVersion: plugin.ProtocolVersion}, &m)
	return m, err
}

// Settings is a project's configuration of one plugin. Secret settings left out keep their
// stored value; an empty string removes one.
type Settings struct {
	Enabled bool
	Config  map[string]any
}

// View is a project's plugin as the API shows it: never secret values.
type View struct {
	Name           string
	Version        string
	Enabled        bool
	DisabledReason *string
	Running        bool
	Capabilities   []string
	Hooks          []string
	UI             []string
	Config         map[string]any
	SecretsSet     []string
	ConfigSchema   json.RawMessage
}

// Configure enables, disables or reconfigures a plugin for a project.
func (h *Host) Configure(ctx context.Context, projectID pgtype.UUID, name string, s Settings) (View, error) {
	q := db.New(h.pool)
	p, err := q.GetPluginByName(ctx, name)
	if err != nil {
		return View{}, err
	}
	var m plugin.Manifest
	_ = json.Unmarshal(p.Manifest, &m)
	schema, err := plugin.ParseConfigSchema(m.ConfigSchema)
	if err != nil {
		return View{}, err
	}
	cfg := s.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	existing := map[string]string{}
	if cur, err := q.GetProjectPlugin(ctx, db.GetProjectPluginParams{ProjectID: projectID, Name: name}); err == nil {
		if existing, err = h.decryptSecrets(cur.Secrets); err != nil {
			return View{}, err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return View{}, err
	}
	public, sec := schema.Split(cfg)
	for k, v := range existing {
		if _, given := cfg[k]; !given && schema.IsSecret(k) {
			sec[k] = v
		}
	}
	check := map[string]any{}
	for k, v := range public {
		check[k] = v
	}
	for k := range sec {
		check[k] = "set"
	}
	if err := schema.Validate(check); err != nil {
		return View{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	enc := ""
	if len(sec) > 0 {
		if h.keys == nil {
			return View{}, ErrNoKeyring
		}
		b, _ := json.Marshal(sec)
		if enc, err = h.keys.Encrypt(b); err != nil {
			return View{}, err
		}
	}
	pb, _ := json.Marshal(public)
	if _, err := q.UpsertProjectPlugin(ctx, db.UpsertProjectPluginParams{ProjectID: projectID, PluginID: p.ID, Config: pb, Secrets: enc, Enabled: s.Enabled}); err != nil {
		return View{}, err
	}
	if s.Enabled {
		h.clearDisabledAlert(ctx, projectID, name)
	}
	if err := h.Sync(ctx); err != nil {
		return View{}, err
	}
	return h.ProjectPlugin(ctx, projectID, name)
}

// ProjectPlugins lists a project's plugins.
func (h *Host) ProjectPlugins(ctx context.Context, projectID pgtype.UUID) ([]View, error) {
	rows, err := db.New(h.pool).ListProjectPlugins(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(rows))
	for _, r := range rows {
		out = append(out, h.view(db.GetProjectPluginRow(r)))
	}
	return out, nil
}

// ProjectPlugin is one project's plugin.
func (h *Host) ProjectPlugin(ctx context.Context, projectID pgtype.UUID, name string) (View, error) {
	r, err := db.New(h.pool).GetProjectPlugin(ctx, db.GetProjectPluginParams{ProjectID: projectID, Name: name})
	if err != nil {
		return View{}, err
	}
	return h.view(r), nil
}

func (h *Host) view(r db.GetProjectPluginRow) View {
	var m plugin.Manifest
	_ = json.Unmarshal(r.Manifest, &m)
	v := View{Name: r.PluginName, Version: m.Version, Enabled: r.Enabled && r.PluginEnabled, DisabledReason: r.DisabledReason,
		Capabilities: m.Capabilities, Hooks: m.Hooks, UI: m.UI, Config: map[string]any{}, SecretsSet: []string{}, ConfigSchema: m.ConfigSchema}
	_ = json.Unmarshal(r.Config, &v.Config)
	if sec, err := h.decryptSecrets(r.Secrets); err == nil {
		for k := range sec {
			v.SecretsSet = append(v.SecretsSet, k)
		}
	}
	if i := h.get(r.ID); i != nil {
		v.Running = i.running()
	}
	return v
}

// Calls lists a project's recent calls to and from a plugin.
func (h *Host) Calls(ctx context.Context, projectID pgtype.UUID, name string, limit int) ([]db.PluginCall, error) {
	q := db.New(h.pool)
	r, err := q.GetProjectPlugin(ctx, db.GetProjectPluginParams{ProjectID: projectID, Name: name})
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return q.ListPluginCalls(ctx, db.ListPluginCallsParams{ProjectPluginID: r.ID, Limit: int32(limit)})
}

// Plugins lists installed plugins.
func (h *Host) Plugins(ctx context.Context) ([]db.Plugin, error) {
	return db.New(h.pool).ListPlugins(ctx)
}
