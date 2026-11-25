package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/plugin"
)

// A core method of an extension is served only to a plugin that declares its capability, and as
// the plugin and project the call came from — whatever the call says.
func TestExtensionMethodsNeedTheirCapability(t *testing.T) {
	var got struct {
		project pgtype.UUID
		plugin  string
	}
	project := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	h := &Host{cfg: Config{Extensions: []Extension{{Capability: plugin.CapabilityMessaging, Methods: map[string]func(context.Context, pgtype.UUID, string, json.RawMessage) (any, error){
		plugin.CoreMessagePost: func(_ context.Context, p pgtype.UUID, name string, _ json.RawMessage) (any, error) {
			got.project, got.plugin = p, name
			return "ok", nil
		},
	}}}}}
	bridge := &instance{h: h, b: binding{projectID: project, name: "slack", manifest: plugin.Manifest{Capabilities: []string{plugin.CapabilityMessaging}}}}
	if r, err := bridge.core(context.Background(), plugin.CoreMessagePost, json.RawMessage(`{"project_id":"someone else's"}`)); err != nil || r != "ok" {
		t.Fatalf("%v %v", r, err)
	}
	if got.project != project || got.plugin != "slack" {
		t.Errorf("served as %v %q", got.project, got.plugin)
	}
	tracker := &instance{h: h, b: binding{projectID: project, name: "linear", manifest: plugin.Manifest{Capabilities: []string{"tracker"}}}}
	_, err := tracker.core(context.Background(), plugin.CoreMessagePost, json.RawMessage(`{}`))
	if pe, ok := errors.AsType[*plugin.Error](err); !ok || pe.Code != plugin.CodeForbidden {
		t.Errorf("a plugin without messaging posted: %v", err)
	}
}

// A plugin's answer to a sender is a success, JSON or plain text, bounded; anything else is the
// usual answer.
func TestWebhookReply(t *testing.T) {
	for _, c := range []struct {
		in       plugin.WebhookResult
		status   int
		ct, body string
	}{
		{plugin.WebhookResult{Body: `{"challenge":"abc"}`}, 200, "application/json", `{"challenge":"abc"}`},
		{plugin.WebhookResult{ContentType: "text/plain", Body: "abc"}, 200, "text/plain; charset=utf-8", "abc"},
		{plugin.WebhookResult{Status: 302, ContentType: "text/html", Body: "<script>"}, 200, "application/json", "<script>"},
		{plugin.WebhookResult{Body: "not json"}, 200, "application/json", `{"status":"ok"}` + "\n"},
	} {
		w := httptest.NewRecorder()
		writeReply(w, c.in)
		if w.Code != c.status || w.Header().Get("Content-Type") != c.ct {
			t.Errorf("%+v: %d %q", c.in, w.Code, w.Header().Get("Content-Type"))
		}
		if c.in.Status != 302 && w.Body.String() != c.body {
			t.Errorf("%+v: body %q", c.in, w.Body.String())
		}
	}
}
