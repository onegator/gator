package proto

import (
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	b, err := Encode(TypeRegister, 7, Register{Name: "mac", Location: "mac", Capabilities: Capabilities{Backends: []string{"claude"}, MaxParallel: 2}})
	if err != nil {
		t.Fatal(err)
	}
	env, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeRegister || env.Seq != 7 || env.Version != Version {
		t.Fatalf("envelope: %+v", env)
	}
	var r Register
	if err := env.Into(&r); err != nil || r.Name != "mac" || r.Capabilities.MaxParallel != 2 {
		t.Fatalf("payload: %+v %v", r, err)
	}
}

func TestDecodeRejectsTypeless(t *testing.T) {
	if _, err := Decode([]byte(`{"seq":1}`)); err == nil {
		t.Fatal("typeless message accepted")
	}
}

func TestNegotiate(t *testing.T) {
	if v, err := Negotiate(Version); err != nil || v != Version {
		t.Fatalf("same version: %d %v", v, err)
	}
	if v, err := Negotiate(Version + 5); err != nil || v != Version {
		t.Fatalf("newer runner should get server version: %d %v", v, err)
	}
	if _, err := Negotiate(MinSupportedVersion - 1); err == nil {
		t.Fatal("too old runner accepted")
	}
}

func TestUsageReported(t *testing.T) {
	if (Usage{}).Reported() {
		t.Fatal("empty usage reported")
	}
	if !(Usage{DurationMS: 1}).Reported() || !(Usage{OutputTokens: 1}).Reported() {
		t.Fatal("measured usage not reported")
	}
}

func TestTimingInvariant(t *testing.T) {
	// A lease must survive at least a few missed heartbeats, and offline must trigger before
	// a lease expires so the dispatcher never hands a job to a runner it thinks is alive.
	if LeaseTTL < 3*HeartbeatInterval || OfflineAfter >= LeaseTTL {
		t.Fatalf("timing: heartbeat %s offline %s lease %s", HeartbeatInterval, OfflineAfter, LeaseTTL)
	}
}

// A v3 job read by a v2 runner decodes as the phase job it is: the new fields are only added,
// and a job without them is a work-tier phase job.
func TestV3JobIsReadableWithoutItsNewFields(t *testing.T) {
	b, err := Encode(TypeLease, 1, Lease{Jobs: []Job{{JobID: "j", Kind: KindReply, Tier: TierChat,
		Agent: &Agent{Handle: "reviewer", DisplayName: "Reviewer"}, Turn: &Turn{ID: "t", Ask: "why?"}}}})
	if err != nil {
		t.Fatal(err)
	}
	var v2 struct {
		Jobs []struct {
			JobID string `json:"job_id"`
		} `json:"jobs"`
	}
	env, _ := Decode(b)
	if err := env.Into(&v2); err != nil || len(v2.Jobs) != 1 || v2.Jobs[0].JobID != "j" {
		t.Fatalf("an older reader: %+v %v", v2, err)
	}
	var v3 Lease
	if err := env.Into(&v3); err != nil || v3.Jobs[0].Turn.Ask != "why?" || !v3.Jobs[0].ReadOnly() || v3.Jobs[0].KindOrPhase() != KindReply {
		t.Fatalf("a v3 reader: %+v %v", v3, err)
	}
	old := Job{JobID: "old"}
	if old.KindOrPhase() != KindPhase || old.ReadOnly() {
		t.Fatal("a job from an older server is a work-tier phase job")
	}
	if v, err := Negotiate(Version - 1); err != nil || v != Version-1 {
		t.Fatalf("the server speaks N-1: %d %v", v, err)
	}
}
