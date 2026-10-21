package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestDispatchHooks(t *testing.T) {
	ctx := context.Background()
	var heard string
	h := &Handlers{
		Webhook: func(_ context.Context, _ *Core, p WebhookParams) error {
			heard = p.DeliveryID
			return nil
		},
		GateEvaluate: func(_ context.Context, _ *Core, p GateEvaluateParams) (GateEvaluateResult, error) {
			return GateEvaluateResult{Checks: []Check{{Name: p.Phase, Status: "pass"}}}, nil
		},
		Schedule: func(context.Context, *Core, ScheduleParams) error { return errors.New("down") },
	}
	core := &Core{}

	res, err := dispatch(ctx, h, core, MethodWebhook, json.RawMessage(`{"delivery_id":"push"}`))
	if err != nil || res != nil || heard != "push" {
		t.Fatalf("webhook: res=%v err=%v heard=%q", res, err, heard)
	}
	res, err = dispatch(ctx, h, core, MethodGateEvaluate, json.RawMessage(`{"phase":"review"}`))
	if r, ok := res.(GateEvaluateResult); err != nil || !ok || len(r.Checks) != 1 || r.Checks[0].Name != "review" {
		t.Fatalf("gate: res=%#v err=%v", res, err)
	}
	if _, err := dispatch(ctx, h, core, MethodSchedule, nil); err == nil || err.Error() != "down" {
		t.Fatalf("schedule error should pass through, got %v", err)
	}

	var re *Error
	if _, err := dispatch(ctx, h, core, MethodRenderUI, nil); !errors.As(err, &re) || re.Code != CodeMethodNotFound || re.Message != `hook "renderUI" not handled` {
		t.Fatalf("unhandled hook: %v", err)
	}
	if _, err := dispatch(ctx, h, core, MethodGateEvaluate, json.RawMessage(`[`)); !errors.As(err, &re) || re.Code != CodeInvalidParams {
		t.Fatalf("bad params: %v", err)
	}
	if _, err := dispatch(ctx, h, core, "nope", nil); !errors.As(err, &re) || re.Code != CodeMethodNotFound || re.Message != `method "nope" not found` {
		t.Fatalf("unknown method: %v", err)
	}
}

// A connection's token lands in a setting; a public one would put it in every API answer.
func TestConnectStoresOnlyIntoSecrets(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"token":{"type":"string","x-secret":true},"repo":{"type":"string"}}}`)
	ok := ConnectSpec{AuthorizeURL: "https://example.com/a", TokenURL: "https://example.com/t", Secret: "token"}
	for name, c := range map[string]ConnectSpec{
		"public token":    {AuthorizeURL: ok.AuthorizeURL, TokenURL: ok.TokenURL, Secret: "repo"},
		"no token":        {AuthorizeURL: ok.AuthorizeURL, TokenURL: ok.TokenURL},
		"plain http":      {AuthorizeURL: "http://example.com/a", TokenURL: ok.TokenURL, Secret: "token"},
		"public webhook":  {AuthorizeURL: ok.AuthorizeURL, TokenURL: ok.TokenURL, Secret: "token", WebhookSecret: "repo"},
		"odd provider":    {AuthorizeURL: ok.AuthorizeURL, TokenURL: ok.TokenURL, Secret: "token", Provider: "Git Hub"},
		"unknown setting": {AuthorizeURL: ok.AuthorizeURL, TokenURL: ok.TokenURL, Secret: "nope"},
	} {
		m := Manifest{Name: "x", Version: "1", ConfigSchema: schema, Connect: &c}
		if m.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	m := Manifest{Name: "x", Version: "1", ConfigSchema: schema, Connect: &ok}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if ok.ProviderName("x") != "x" {
		t.Fatal("the provider defaults to the plugin's name")
	}
}
