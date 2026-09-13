package secrets

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestEncryptDecryptRotate(t *testing.T) {
	k1 := NewKey("k1")
	k2 := NewKey("k2")
	old, err := ParseKeyring(k1, "")
	if err != nil {
		t.Fatal(err)
	}
	ct, err := old.Encrypt([]byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ct, "v1.k1.") || strings.Contains(ct, "hunter2") {
		t.Fatalf("ciphertext %q", ct)
	}
	// new keyring with k2 current and k1 old still decrypts and rotates
	ring, err := ParseKeyring(k2, k1)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := ring.Decrypt(ct)
	if err != nil || string(pt) != "hunter2" {
		t.Fatalf("decrypt with old key: %q %v", pt, err)
	}
	if !ring.NeedsRotation(ct) {
		t.Fatal("should need rotation")
	}
	rotated, err := ring.Rotate(ct)
	if err != nil || KeyID(rotated) != "k2" || ring.NeedsRotation(rotated) {
		t.Fatalf("rotate: %q %v", rotated, err)
	}
	// keyring without k1 cannot read the old ciphertext
	only2, _ := ParseKeyring(k2, "")
	if _, err := only2.Decrypt(ct); !errors.Is(err, ErrNoKey) {
		t.Fatalf("missing key: %v", err)
	}
	// tampering fails
	if _, err := ring.Decrypt(ct[:len(ct)-2] + "AA"); err == nil {
		t.Fatal("tampered ciphertext decrypted")
	}
}

func TestParseKeyringRejectsBadKeys(t *testing.T) {
	if _, err := ParseKeyring("", ""); err == nil {
		t.Fatal("empty key accepted")
	}
	if _, err := ParseKeyring("k:abcd", ""); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestRedaction(t *testing.T) {
	m := RedactMap(map[string]any{"name": "gh", "api_token": "x", "nested": map[string]any{"client_secret": "y", "url": "u"}})
	if m["api_token"] != Redacted || m["nested"].(map[string]any)["client_secret"] != Redacted || m["name"] != "gh" {
		t.Fatalf("redact map: %v", m)
	}
	if out := string(RedactJSON([]byte(`{"password":"p","ok":1}`))); strings.Contains(out, `"p"`) {
		t.Fatalf("redact json: %s", out)
	}
	var buf bytes.Buffer
	log := slog.New(RedactingHandler{Inner: slog.NewJSONHandler(&buf, nil)})
	log.With("token", "abc").Info("hello", "password", "zzz", "user", "bob", slog.Group("g", "secret", "q"))
	_ = context.Background()
	out := buf.String()
	for _, leaked := range []string{"abc", "zzz", `"q"`} {
		if strings.Contains(out, leaked) {
			t.Fatalf("log leaked %s: %s", leaked, out)
		}
	}
	if !strings.Contains(out, "bob") {
		t.Fatalf("non-secret dropped: %s", out)
	}
}
