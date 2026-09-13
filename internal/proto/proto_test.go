package proto

import (
	"encoding/json"
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	in := Envelope{Type: TypeRegister, Seq: 1, Version: Version, Payload: Register{Name: "mac", Location: "mac"}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Envelope
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != TypeRegister || out.Seq != 1 || out.Version != Version {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestVersionWindow(t *testing.T) {
	if MinSupportedVersion > Version {
		t.Fatalf("MinSupportedVersion %d > Version %d", MinSupportedVersion, Version)
	}
}
