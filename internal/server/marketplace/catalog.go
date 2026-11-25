package marketplace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// catalogStore is what the catalogue reads and writes. *db.Queries is one.
type catalogStore interface {
	ListRegistries(ctx context.Context) ([]db.MarketplaceRegistry, error)
	GetRegistry(ctx context.Context, id pgtype.UUID) (db.MarketplaceRegistry, error)
	CreateRegistry(ctx context.Context, arg db.CreateRegistryParams) (db.MarketplaceRegistry, error)
	EnsureBuiltinRegistry(ctx context.Context, arg db.EnsureBuiltinRegistryParams) (db.MarketplaceRegistry, error)
	DeleteRegistry(ctx context.Context, id pgtype.UUID) (int64, error)
	CountRegistryInstallations(ctx context.Context, registryID pgtype.UUID) (int32, error)
	SetRegistryFetched(ctx context.Context, arg db.SetRegistryFetchedParams) error
	SetRegistryFetchError(ctx context.Context, arg db.SetRegistryFetchErrorParams) error
	GetItemChecksum(ctx context.Context, arg db.GetItemChecksumParams) (string, error)
	UpsertItem(ctx context.Context, arg db.UpsertItemParams) error
	UnlistItems(ctx context.Context, arg db.UnlistItemsParams) error
	ListItems(ctx context.Context, kind *string) ([]db.ListItemsRow, error)
	ListItemVersions(ctx context.Context, arg db.ListItemVersionsParams) ([]db.ListItemVersionsRow, error)
	GetItem(ctx context.Context, arg db.GetItemParams) (db.MarketplaceItem, error)
}

// Catalog is every registry's packages, as their last fetch found them: what people browse, and
// where installing takes a package from.
type Catalog struct {
	store   catalogStore
	fetcher Fetcher
	kinds   Kinds
	limits  Limits
}

// NewCatalog builds it.
func NewCatalog(store catalogStore, fetcher Fetcher, kinds Kinds, lim Limits) *Catalog {
	return &Catalog{store: store, fetcher: fetcher, kinds: kinds, limits: lim}
}

// Builtin is Gator's own registry, as the server is configured (GATOR_MARKETPLACE_URL and
// GATOR_MARKETPLACE_PUBLIC_KEY).
type Builtin struct {
	Name, URL, PublicKey string
}

// EnsureBuiltin brings the built-in registry's row in line with the configuration.
func (c *Catalog) EnsureBuiltin(ctx context.Context, b Builtin) (db.MarketplaceRegistry, error) {
	if b.Name == "" {
		b.Name = "gator"
	}
	if b.PublicKey != "" {
		if _, err := ParsePublicKeys(b.PublicKey); err != nil {
			return db.MarketplaceRegistry{}, err
		}
	}
	return c.store.EnsureBuiltinRegistry(ctx, db.EnsureBuiltinRegistryParams{Name: b.Name, Url: b.URL, PublicKey: b.PublicKey})
}

// Registries lists the registries, Gator's first. Anyone signed in sees them.
func (c *Catalog) Registries(ctx context.Context, p auth.Principal) ([]db.MarketplaceRegistry, error) {
	if p.Kind != auth.KindUser {
		return nil, ErrForbidden
	}
	return c.store.ListRegistries(ctx)
}

// AddRegistry adds a company's own registry. Workspace admins only: what a registry offers can
// end up in every project's prompts.
func (c *Catalog) AddRegistry(ctx context.Context, p auth.Principal, name, url, publicKey string) (db.MarketplaceRegistry, error) {
	if !p.IsWorkspaceAdmin() {
		return db.MarketplaceRegistry{}, ErrForbidden
	}
	if !ValidName(name) {
		return db.MarketplaceRegistry{}, invalid("a registry's name is lowercase letters, digits and dashes")
	}
	if strings.TrimSpace(url) == "" {
		return db.MarketplaceRegistry{}, invalid("a registry needs an address")
	}
	if publicKey != "" {
		if _, err := ParsePublicKeys(publicKey); err != nil {
			return db.MarketplaceRegistry{}, err
		}
	}
	r, err := c.store.CreateRegistry(ctx, db.CreateRegistryParams{Name: name, Url: url, PublicKey: publicKey, CreatedBy: p.UserID})
	if pe, ok := errors.AsType[*pgconn.PgError](err); ok && pe.Code == "23505" {
		return r, fmt.Errorf("%w: there is a registry named %s", ErrConflict, name)
	}
	return r, err
}

// DeleteRegistry removes a registry that nothing is installed from. Gator's own stays.
func (c *Catalog) DeleteRegistry(ctx context.Context, p auth.Principal, id pgtype.UUID) error {
	if !p.IsWorkspaceAdmin() {
		return ErrForbidden
	}
	r, err := c.store.GetRegistry(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if r.Builtin {
		return fmt.Errorf("%w: Gator's own registry stays; point GATOR_MARKETPLACE_URL elsewhere instead", ErrConflict)
	}
	if n, err := c.store.CountRegistryInstallations(ctx, id); err != nil {
		return err
	} else if n > 0 {
		return fmt.Errorf("%w: %d installations come from this registry; remove them first", ErrConflict, n)
	}
	_, err = c.store.DeleteRegistry(ctx, id)
	return err
}

// FetchResult is what refreshing a registry did.
type FetchResult struct {
	Registry db.MarketplaceRegistry
	Fetched  []Item
	Rejected []Rejection
}

// Refresh fetches a registry and caches what verifies. A signature that does not verify takes
// nothing; a package that does not match its checksum, breaks a limit or does not read is turned
// away on its own, and so is a version already published with other files. Nothing installed
// changes: an update is always a person's.
func (c *Catalog) Refresh(ctx context.Context, p auth.Principal, id pgtype.UUID) (FetchResult, error) {
	if !p.IsWorkspaceAdmin() {
		return FetchResult{}, ErrForbidden
	}
	return c.refresh(ctx, id)
}

func (c *Catalog) refresh(ctx context.Context, id pgtype.UUID) (FetchResult, error) {
	r, err := c.store.GetRegistry(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return FetchResult{}, ErrNotFound
	}
	if err != nil {
		return FetchResult{}, err
	}
	res := FetchResult{Registry: r}
	contents, err := c.read(ctx, r)
	if err != nil {
		_ = c.store.SetRegistryFetchError(ctx, db.SetRegistryFetchErrorParams{ID: id, FetchError: err.Error()})
		return res, err
	}
	res.Rejected = contents.Rejected
	keep := []string{}
	for _, pkg := range contents.Packages {
		if err := c.store1(ctx, r.ID, pkg); err != nil {
			if !errors.Is(err, ErrChecksum) {
				return res, err
			}
			res.Rejected = append(res.Rejected, Rejection{Item: pkg.Item, Reason: err.Error()})
			continue
		}
		res.Fetched = append(res.Fetched, pkg.Item)
		keep = append(keep, pkg.Item.Key()+"@"+pkg.Item.Version)
	}
	if err := c.store.UnlistItems(ctx, db.UnlistItemsParams{RegistryID: r.ID, Keep: keep}); err != nil {
		return res, err
	}
	var why []string
	for _, rej := range res.Rejected {
		why = append(why, rej.Reason)
	}
	if err := c.store.SetRegistryFetched(ctx, db.SetRegistryFetchedParams{ID: r.ID, FetchError: strings.Join(why, "; ")}); err != nil {
		return res, err
	}
	res.Registry, err = c.store.GetRegistry(ctx, id)
	return res, err
}

func (c *Catalog) read(ctx context.Context, r db.MarketplaceRegistry) (Contents, error) {
	v, err := NewVerifier(r.PublicKey, c.limits)
	if err != nil {
		return Contents{}, err
	}
	snap, err := c.fetcher.Fetch(ctx, r.Url)
	if err != nil {
		return Contents{}, err
	}
	defer snap.Close()
	return Read(snap.FS, v, c.kinds, c.limits)
}

// store1 caches one package, unless its version was published before with other files: a
// version is a promise about its files, and one that changed under the same number is what a
// compromised or careless registry looks like.
func (c *Catalog) store1(ctx context.Context, registry pgtype.UUID, pkg Package) error {
	it := pkg.Item
	prev, err := c.store.GetItemChecksum(ctx, db.GetItemChecksumParams{RegistryID: registry, Kind: it.Kind, Name: it.Name, Version: it.Version})
	switch {
	case err == nil && prev != it.Checksum:
		return fmt.Errorf("%w: %s %s was published with other files (%s); a published version does not change", ErrChecksum, it.Key(), it.Version, prev)
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	manifest, _ := json.Marshal(pkg.Manifest)
	files, _ := json.Marshal(pkg.Files)
	return c.store.UpsertItem(ctx, db.UpsertItemParams{RegistryID: registry, Kind: it.Kind, Name: it.Name, Version: it.Version,
		Manifest: manifest, Checksum: it.Checksum, Files: files})
}

// Listing is a package as the catalogue offers it: its newest listed version.
type Listing struct {
	RegistryID   pgtype.UUID
	RegistryName string
	Manifest     Manifest
	Checksum     string
	Versions     []string // every listed version, newest first
}

// Filter narrows the catalogue.
type Filter struct {
	Kind  string
	Query string // words in the name, display name or description
	Phase string
}

// List is the catalogue: every package of every registry at its newest listed version.
func (c *Catalog) List(ctx context.Context, p auth.Principal, f Filter) ([]Listing, error) {
	if p.Kind != auth.KindUser {
		return nil, ErrForbidden
	}
	var kind *string
	if f.Kind != "" {
		kind = &f.Kind
	}
	rows, err := c.store.ListItems(ctx, kind)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*Listing{}
	var order []string
	for _, row := range rows {
		var m Manifest
		if err := json.Unmarshal(row.Manifest, &m); err != nil {
			continue
		}
		key := row.RegistryName + "/" + row.Kind + "/" + row.Name
		l, ok := byKey[key]
		if !ok {
			l = &Listing{RegistryID: row.RegistryID, RegistryName: row.RegistryName}
			byKey[key] = l
			order = append(order, key)
		}
		l.Versions = append(l.Versions, row.Version)
		if l.Manifest.Version == "" || CompareVersions(row.Version, l.Manifest.Version) > 0 {
			l.Manifest, l.Checksum = m, row.Checksum
		}
	}
	out := []Listing{}
	for _, key := range order {
		l := byKey[key]
		slices.SortFunc(l.Versions, func(a, b string) int { return CompareVersions(b, a) })
		if f.matches(l.Manifest) {
			out = append(out, *l)
		}
	}
	return out, nil
}

func (f Filter) matches(m Manifest) bool {
	if f.Phase != "" && !slices.Contains(m.Phases, f.Phase) {
		return false
	}
	q := strings.ToLower(strings.TrimSpace(f.Query))
	if q == "" {
		return true
	}
	text := strings.ToLower(m.Name + " " + m.DisplayName + " " + m.Description)
	for w := range strings.FieldsSeq(q) {
		if !strings.Contains(text, w) {
			return false
		}
	}
	return true
}

// Version is one version of a package, as the details screen lists it.
type Version struct {
	RegistryID   pgtype.UUID
	RegistryName string
	Manifest     Manifest
	Checksum     string
	Listed       bool
}

// Versions are every version of a package the registries have offered, newest first; registry
// narrows them to one registry (zero: all).
func (c *Catalog) Versions(ctx context.Context, p auth.Principal, kind, name string, registry pgtype.UUID) ([]Version, error) {
	if p.Kind != auth.KindUser {
		return nil, ErrForbidden
	}
	rows, err := c.store.ListItemVersions(ctx, db.ListItemVersionsParams{Kind: kind, Name: name, RegistryID: registry})
	if err != nil {
		return nil, err
	}
	var out []Version
	for _, row := range rows {
		var m Manifest
		if err := json.Unmarshal(row.Manifest, &m); err != nil {
			continue
		}
		out = append(out, Version{RegistryID: row.RegistryID, RegistryName: row.RegistryName, Manifest: m, Checksum: row.Checksum, Listed: row.Listed})
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	slices.SortStableFunc(out, func(a, b Version) int { return CompareVersions(b.Manifest.Version, a.Manifest.Version) })
	return out, nil
}

// Package is one cached version, its files checked against its checksum once more: what was
// verified at fetch is verified again before anything of it is installed.
func (c *Catalog) Package(ctx context.Context, registry pgtype.UUID, kind, name, version string) (Package, error) {
	row, err := c.store.GetItem(ctx, db.GetItemParams{RegistryID: registry, Kind: kind, Name: name, Version: version})
	if errors.Is(err, pgx.ErrNoRows) {
		return Package{}, ErrNotFound
	}
	if err != nil {
		return Package{}, err
	}
	pkg := Package{Item: Item{Kind: row.Kind, Name: row.Name, Version: row.Version, Checksum: row.Checksum}}
	if err := json.Unmarshal(row.Manifest, &pkg.Manifest); err != nil {
		return pkg, err
	}
	if err := json.Unmarshal(row.Files, &pkg.Files); err != nil {
		return pkg, err
	}
	if err := (Checksums{Limits: c.limits}).VerifyPackage(pkg.Item, pkg.Files); err != nil {
		return pkg, err
	}
	return pkg, nil
}
