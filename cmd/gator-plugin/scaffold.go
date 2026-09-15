package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var pluginName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// scaffold writes a working plugin: main.go, a scenario and a README. It never overwrites.
func scaffold(name, dir string) ([]string, error) {
	if !pluginName.MatchString(name) {
		return nil, fmt.Errorf("plugin name %q must be lowercase letters, digits and dashes", name)
	}
	files := map[string]string{
		"main.go":              mainTemplate,
		"scenarios/basic.json": scenarioTemplate,
		"README.md":            readmeTemplate,
	}
	var written []string
	for rel := range files {
		if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
			return nil, fmt.Errorf("%s already exists", filepath.Join(dir, rel))
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	for _, rel := range []string{"main.go", "scenarios/basic.json", "README.md"} {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return written, err
		}
		body := strings.ReplaceAll(files[rel], "{{name}}", name)
		body = strings.ReplaceAll(body, "{{ref}}", strings.ReplaceAll(name, "-", "_")+"_id")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return written, err
		}
		written = append(written, path)
	}
	return written, nil
}

const mainTemplate = `// Command {{name}} is a Gator plugin. The protocol is in gator's docs/plugins.md.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/onegator/gator/plugin"
)

func main() {
	err := plugin.Serve(plugin.Handlers{
		Manifest: plugin.Manifest{
			Name:         "{{name}}",
			Version:      "0.1.0",
			Capabilities: []string{"tracker"},
			ConfigSchema: json.RawMessage(` + "`" + `{"type":"object","properties":{
				"webhook_secret":{"type":"string","x-secret":true,"description":"key the sender signs deliveries with"}},
				"required":["webhook_secret"]}` + "`" + `),
			Webhook: &plugin.WebhookSpec{DeliveryHeader: "X-Delivery-Id"},
		},
		Webhook:         webhook,
		PhaseTransition: phaseTransition,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// webhook turns an incoming event into a task. Deliveries can repeat, so it looks before it
// creates; a bad signature is refused with CodeForbidden, which does not count as a failure.
func webhook(ctx context.Context, core *plugin.Core, p plugin.WebhookParams) error {
	m := hmac.New(sha256.New, []byte(core.Secret("webhook_secret")))
	m.Write(p.Body)
	if !hmac.Equal([]byte(p.Headers["X-Signature"]), []byte("sha256="+hex.EncodeToString(m.Sum(nil)))) {
		return plugin.Errorf(plugin.CodeForbidden, "bad signature")
	}
	var event struct {
		ID    string ` + "`json:\"id\"`" + `
		Title string ` + "`json:\"title\"`" + `
	}
	if err := json.Unmarshal(p.Body, &event); err != nil || event.ID == "" {
		return plugin.Errorf(plugin.CodeInvalidParams, "body needs id and title")
	}
	if t, err := core.FindTask(ctx, "{{ref}}", event.ID); err != nil || t != nil {
		return err
	}
	_, err := core.CreateTask(ctx, plugin.TaskCreateParams{Kind: "bug", Title: event.Title, ExternalRefs: map[string]string{"{{ref}}": event.ID}})
	return err
}

func phaseTransition(ctx context.Context, core *plugin.Core, p plugin.PhaseTransitionParams) error {
	return core.Log(ctx, "info", fmt.Sprintf("%s: %s -> %s", p.Task.Title, p.From, p.To), nil)
}
`

const scenarioTemplate = `{
  "project": {"slug": "demo"},
  "secrets": {"webhook_secret": "dev-secret"},
  "tasks": [{"id": "t1", "kind": "feature", "title": "Search", "phase": "planning"}],
  "steps": [
    {"hook": "webhook", "body": {"id": "1", "title": "Crash on login"},
     "sign": {"header": "X-Signature", "secret": "webhook_secret", "prefix": "sha256="},
     "expect_core": ["task.create"]},
    {"hook": "webhook", "body": {"id": "1", "title": "Crash on login"},
     "sign": {"header": "X-Signature", "secret": "webhook_secret", "prefix": "sha256="},
     "expect_core": ["task.find"]},
    {"hook": "webhook", "body": {"id": "2", "title": "Forged"}, "headers": {"X-Signature": "sha256=00"},
     "expect_error": true},
    {"hook": "phaseTransition", "task": "t1", "from": "planning", "to": "implementation", "expect_core": ["log"]}
  ]
}
`

const readmeTemplate = "# {{name}}\n\nA Gator plugin. Build and play its scenario against an in-memory core:\n\n" +
	"```sh\ngo build -o bin/{{name}} .\ngator-plugin dev scenarios/basic.json -- ./bin/{{name}}\n```\n\n" +
	"Install on a server (workspace admin):\n\n" +
	"```sh\ncurl -X PUT -H \"Authorization: Bearer $TOKEN\" $GATOR/api/v1/plugins/{{name}} -d '{\"command\":[\"/usr/local/lib/gator/plugins/{{name}}\"]}'\n```\n"
