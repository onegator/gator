package marketplace

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// The registry signs its index.json with Ed25519, and nothing else: the index carries every
// package's checksum, so one signature over it covers every file of every package. Plain
// Ed25519 over the file's bytes is the whole scheme — no certificates, no transparency log, no
// dependency beyond the standard library — which is what a curated registry with one key (and
// one more while it rotates) needs. Keys and signatures are base64 with a scheme prefix, so a
// second scheme could come later without guessing.
const (
	publicKeyPrefix  = "ed25519:"
	privateKeyPrefix = "ed25519-seed:"
)

// IndexVerifier checks a registry's signature over its index.
type IndexVerifier interface {
	VerifyIndex(index, signature []byte) error
}

// PackageVerifier checks a package's files against what its index promised.
type PackageVerifier interface {
	VerifyPackage(it Item, files Files) error
}

// Verifier checks what a Fetcher brought, before any of it is read further.
type Verifier interface {
	IndexVerifier
	PackageVerifier
}

type verifier struct {
	IndexVerifier
	PackageVerifier
}

// NewVerifier is a registry's verifier: its public keys (none: the registry is unsigned, and
// only checksums hold) and the server's limits.
func NewVerifier(publicKeys string, lim Limits) (Verifier, error) {
	var iv IndexVerifier = Unsigned{}
	if strings.TrimSpace(publicKeys) != "" {
		keys, err := ParsePublicKeys(publicKeys)
		if err != nil {
			return nil, err
		}
		iv = keys
	}
	return verifier{IndexVerifier: iv, PackageVerifier: Checksums{Limits: lim}}, nil
}

// PublicKeys are the keys a registry may sign with; any one verifying is enough, so a key can be
// rotated without a moment when the registry cannot be read.
type PublicKeys []ed25519.PublicKey

// ParsePublicKeys reads "ed25519:<base64>" keys, separated by commas or spaces.
func ParsePublicKeys(s string) (PublicKeys, error) {
	var out PublicKeys
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		b64, ok := strings.CutPrefix(f, publicKeyPrefix)
		if !ok {
			return nil, invalid("a registry key is %q followed by base64", publicKeyPrefix)
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, invalid("a registry key is %d bytes of base64", ed25519.PublicKeySize)
		}
		out = append(out, ed25519.PublicKey(raw))
	}
	if len(out) == 0 {
		return nil, invalid("no registry key")
	}
	return out, nil
}

// VerifyIndex implements IndexVerifier.
func (keys PublicKeys) VerifyIndex(index, signature []byte) error {
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signature)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: %s is missing or not a signature", ErrSignature, SignatureFile)
	}
	for _, k := range keys {
		if ed25519.Verify(k, index, sig) {
			return nil
		}
	}
	return ErrSignature
}

// Unsigned is a registry that has no key: a company's own, which its admin chose not to sign.
// Its checksums still hold.
type Unsigned struct{}

// VerifyIndex implements IndexVerifier.
func (Unsigned) VerifyIndex([]byte, []byte) error { return nil }

// Checksums holds every package to its checksum and to the limits.
type Checksums struct{ Limits Limits }

// VerifyPackage implements PackageVerifier.
func (c Checksums) VerifyPackage(it Item, files Files) error {
	if err := CheckFiles(files, c.Limits); err != nil {
		return fmt.Errorf("%s %s: %w", it.Key(), it.Version, err)
	}
	if files.Checksum() != it.Checksum {
		// Not the hash itself: of a directory registry, that would be a fingerprint of files on the server.
		return fmt.Errorf("%w: the files of %s %s do not hash to what its index promises", ErrChecksum, it.Key(), it.Version)
	}
	return nil
}

// GenerateKey makes a registry key pair, as the public key the server is configured with and the
// private key the registry's maintainer keeps (never in the repository).
func GenerateKey() (public, private string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return publicKeyPrefix + base64.StdEncoding.EncodeToString(pub),
		privateKeyPrefix + base64.StdEncoding.EncodeToString(priv.Seed()), nil
}

// Sign signs an index with a private key from GenerateKey, as index.json.sig holds it.
func Sign(private string, index []byte) ([]byte, error) {
	b64, ok := strings.CutPrefix(strings.TrimSpace(private), privateKeyPrefix)
	if !ok {
		return nil, invalid("a private key is %q followed by base64", privateKeyPrefix)
	}
	seed, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, invalid("a private key is %d bytes of base64", ed25519.SeedSize)
	}
	sig := ed25519.Sign(ed25519.NewKeyFromSeed(seed), index)
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n"), nil
}

// PublicKeyOf answers the public key of a private key, for a maintainer checking they hold the
// key the server expects.
func PublicKeyOf(private string) (string, error) {
	b64, ok := strings.CutPrefix(strings.TrimSpace(private), privateKeyPrefix)
	if !ok {
		return "", invalid("a private key is %q followed by base64", privateKeyPrefix)
	}
	seed, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(seed) != ed25519.SeedSize {
		return "", invalid("a private key is %d bytes of base64", ed25519.SeedSize)
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	return publicKeyPrefix + base64.StdEncoding.EncodeToString(pub), nil
}
