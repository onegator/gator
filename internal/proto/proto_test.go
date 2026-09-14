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
