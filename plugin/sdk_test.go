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
