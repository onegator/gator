// Package marketplacetest builds registries on disk for tests: packages written as files, an
// index from what is there, and a signature with a key made for the test. Nothing of it is a
// real key; nothing is fetched from anywhere but the test's own directory.
package marketplacetest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/onegator/gator/internal/server/marketplace"
)

// Registry is a registry in a test's directory.
type Registry struct {
	t testing.TB
	// Dir is where it is, URL how a server reaches it and PublicKey what verifies it.
	Dir, URL, PublicKey string
	private             string
	serial              int64
}

// New makes an empty registry with a key of its own.
func New(t testing.TB) *Registry {
	t.Helper()
	pub, priv, err := marketplace.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	return &Registry{t: t, Dir: dir, URL: "file://" + dir, PublicKey: pub, private: priv}
}

// Write puts a file into the registry, without publishing it.
func (r *Registry) Write(rel, content string) {
	r.t.Helper()
	p := filepath.Join(r.Dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// Package writes a package's files under its folder.
func (r *Registry) Package(dir string, files map[string]string) {
	r.t.Helper()
	for name, content := range files {
		r.Write(dir+"/"+name, content)
	}
}

// Publish writes the index from the packages there and signs it, as the registry's maintainer
// does.
func (r *Registry) Publish() {
	r.t.Helper()
	idx, err := marketplace.Scan(os.DirFS(r.Dir), marketplace.DefaultKinds(), marketplace.DefaultLimits)
	if err != nil {
		r.t.Fatal(err)
	}
	r.serial++
	idx.Serial = r.serial
	r.WriteIndex(marketplace.MarshalIndex(idx))
}

// WriteIndex writes an index as given and signs it.
func (r *Registry) WriteIndex(index []byte) {
	r.t.Helper()
	r.Write(marketplace.IndexFile, string(index))
	sig, err := marketplace.Sign(r.private, index)
	if err != nil {
		r.t.Fatal(err)
	}
	r.Write(marketplace.SignatureFile, string(sig))
}

// Index reads the published index.
func (r *Registry) Index() []byte {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.Dir, marketplace.IndexFile))
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}
