package marketplace

import (
	"encoding/json"
	"slices"
	"time"
)

// IndexFile and SignatureFile are where a registry keeps its index and the signature over it.
const (
	IndexFile     = "index.json"
	SignatureFile = "index.json.sig"
)

// MaxIndexBytes bounds a registry's index.
const MaxIndexBytes = 1 << 20

// Index is a registry's index.json: every package it offers, where its folder is and what its
// files hash to.
type Index struct {
	// Serial grows with every publication, and Expires (RFC 3339, optional) ends one: both are
	// signed with the rest, so a registry (or whoever sits between it and the server) cannot serve
	// an older index again, or keep serving one forever, to hold a fixed package back.
	Serial  int64  `json:"serial"`
	Expires string `json:"expires,omitempty"`
	Items   []Item `json:"items"`
}

// ExpiresAt is when the index stops being good; zero: it does not say.
func (idx Index) ExpiresAt() time.Time {
	t, _ := time.Parse(time.RFC3339, idx.Expires)
	return t
}

// SameItems says whether two indexes offer the same packages.
func (idx Index) SameItems(other Index) bool { return slices.Equal(idx.Items, other.Items) }

// Item is one package of an index.
type Item struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	Path     string `json:"path"`
	Checksum string `json:"checksum"`
}

// Key names a package of a registry, all versions alike.
func (it Item) Key() string { return it.Kind + "/" + it.Name }

// ParseIndex reads an index and checks every item is well formed: a known kind, a name, a
// version, a path inside the registry and a checksum, each package once.
func ParseIndex(raw []byte, kinds []string) (Index, error) {
	var idx Index
	if len(raw) > MaxIndexBytes {
		return idx, invalid("the index is larger than %d bytes", MaxIndexBytes)
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		return idx, invalid("the index is not JSON: %v", err)
	}
	if idx.Serial < 0 {
		return idx, invalid("the index's serial is negative")
	}
	if idx.Expires != "" && idx.ExpiresAt().IsZero() {
		return idx, invalid("the index's expires %q is not RFC 3339", idx.Expires)
	}
	seen := map[string]bool{}
	for _, it := range idx.Items {
		if !slices.Contains(kinds, it.Kind) {
			return idx, invalid("%s: kind %q is not one this server installs", it.Name, it.Kind)
		}
		if !ValidName(it.Name) {
			return idx, invalid("%q is not a package name", it.Name)
		}
		if !ValidVersion(it.Version) {
			return idx, invalid("%s: %q is not a version", it.Name, it.Version)
		}
		if err := SafePath(it.Path); err != nil {
			return idx, err
		}
		if len(it.Checksum) != len("sha256:")+64 || it.Checksum[:7] != "sha256:" {
			return idx, invalid("%s: %q is not a sha256 checksum", it.Name, it.Checksum)
		}
		if seen[it.Key()] {
			return idx, invalid("%s is listed twice", it.Key())
		}
		seen[it.Key()] = true
	}
	return idx, nil
}
