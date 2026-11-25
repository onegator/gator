package marketplace

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/onegator/gator/internal/server/mcpremote"
)

// IntegrationFile is an integration package's manifest.
const IntegrationFile = "integration.yaml"

// Integration tracks.
const (
	IntegrationMCP    = "mcp"
	IntegrationNative = "native"
)

// Auth of a remote MCP server.
const (
	AuthOAuth  = mcpremote.AuthOAuth
	AuthBearer = mcpremote.AuthBearer
	AuthNone   = mcpremote.AuthNone
)

// IntegrationManifest is integration.yaml: a remote MCP server agents reach through Gator's
// proxy, or a native plugin the server runs.
type IntegrationManifest struct {
	Name              string            `yaml:"name" json:"name"`
	Version           string            `yaml:"version" json:"version"`
	DisplayName       string            `yaml:"display_name" json:"display_name"`
	Description       string            `yaml:"description" json:"description"`
	Kind              string            `yaml:"kind" json:"kind"`
	Phases            []string          `yaml:"phases,omitempty" json:"phases,omitempty"`
	Icon              string            `yaml:"icon,omitempty" json:"icon,omitempty"`
	Capabilities      []string          `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	Permissions       []string          `yaml:"permissions,omitempty" json:"permissions,omitempty"`
	URL               string            `yaml:"url,omitempty" json:"url,omitempty"`
	Auth              string            `yaml:"auth,omitempty" json:"auth,omitempty"`
	OAuth             *IntegrationOAuth `yaml:"oauth,omitempty" json:"oauth,omitempty"`
	ToolsDefaultAllow []string          `yaml:"tools_default_allow,omitempty" json:"tools_default_allow,omitempty"`
	// Binaries are a native plugin's builds, by GOOS/GOARCH: where to download each and what it
	// hashes to. Nothing is made executable before it matches.
	Binaries map[string]Binary `yaml:"binaries,omitempty" json:"binaries,omitempty"`
}

// IntegrationOAuth is how a person connects a remote MCP server that signs in with OAuth: the
// provider's endpoints, and the server's own OAuth app for it (GATOR_<PROVIDER>_CLIENT_ID).
type IntegrationOAuth struct {
	Provider     string   `yaml:"provider" json:"provider"`
	AuthorizeURL string   `yaml:"authorize_url" json:"authorize_url"`
	TokenURL     string   `yaml:"token_url" json:"token_url"`
	Scopes       []string `yaml:"scopes,omitempty" json:"scopes,omitempty"`
}

// Binary is one build of a native plugin.
type Binary struct {
	URL    string `yaml:"url" json:"url"`
	SHA256 string `yaml:"sha256" json:"sha256"`
}

var (
	platformPattern = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+$`)
	sha256Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Integration is the integration kind.
type Integration struct{}

// Kind implements Kind.
func (Integration) Kind() string { return KindIntegration }

// Describe implements Kind.
func (Integration) Describe(_ Item, files Files) (Manifest, error) {
	im, err := ParseIntegration(files)
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{Name: im.Name, Version: im.Version, DisplayName: im.DisplayName, Description: im.Description,
		Phases: im.Phases, Icon: im.Icon, Detail: detail(im)}
	m.Permissions = append(m.Permissions, im.Permissions...)
	for _, c := range im.Capabilities {
		m.Permissions = append(m.Permissions, "gives the project the capability "+c)
	}
	switch im.Kind {
	case IntegrationMCP:
		host := hostOf(im.URL)
		m.Permissions = append(m.Permissions, "agents reach "+host+" through Gator's MCP proxy; its credentials stay on the server")
		switch im.Auth {
		case AuthOAuth:
			m.Permissions = append(m.Permissions, "signs in to "+im.OAuth.Provider+" for the project (OAuth)")
		case AuthBearer:
			m.Permissions = append(m.Permissions, "keeps a token for "+host+" on the server")
		}
		if len(im.ToolsDefaultAllow) > 0 {
			m.Permissions = append(m.Permissions, "agents given it may call: "+strings.Join(im.ToolsDefaultAllow, ", "))
		}
	case IntegrationNative:
		platforms := make([]string, 0, len(im.Binaries))
		for p := range im.Binaries {
			platforms = append(platforms, p)
		}
		slices.Sort(platforms)
		m.Permissions = append(m.Permissions, fmt.Sprintf("runs a native plugin on the server, as the server's user (%s)", strings.Join(platforms, ", ")))
	}
	return m, nil
}

// ParseIntegration reads and checks integration.yaml.
func ParseIntegration(files Files) (IntegrationManifest, error) {
	var im IntegrationManifest
	if err := readYAML(files, IntegrationFile, &im); err != nil {
		return im, err
	}
	for field, v := range map[string]string{"name": im.Name, "version": im.Version, "display_name": im.DisplayName, "description": im.Description} {
		if err := requireText(field, v); err != nil {
			return im, err
		}
	}
	switch im.Kind {
	case IntegrationMCP:
		return im, checkMCP(im)
	case IntegrationNative:
		return im, checkNative(im)
	}
	return im, invalid("kind must be %s or %s, not %q", IntegrationMCP, IntegrationNative, im.Kind)
}

func checkMCP(im IntegrationManifest) error {
	if !mcpremote.ValidName(im.Name) {
		return invalid("%q cannot name a remote MCP server", im.Name)
	}
	if err := CheckServerURL(im.URL); err != nil {
		return err
	}
	if len(im.Binaries) > 0 {
		return invalid("a remote MCP server has no binaries")
	}
	switch im.Auth {
	case AuthNone, AuthBearer:
		if im.OAuth != nil {
			return invalid("oauth is for auth: oauth")
		}
	case AuthOAuth:
		o := im.OAuth
		if o == nil || !ValidName(o.Provider) {
			return invalid("auth: oauth needs oauth.provider, authorize_url and token_url")
		}
		for _, u := range []string{o.AuthorizeURL, o.TokenURL} {
			if err := CheckServerURL(u); err != nil {
				return err
			}
		}
	default:
		return invalid("auth must be oauth, bearer or none, not %q", im.Auth)
	}
	for _, t := range im.ToolsDefaultAllow {
		if !mcpremote.ValidToolName(t) {
			return invalid("%q is not a tool name", t)
		}
	}
	return nil
}

func checkNative(im IntegrationManifest) error {
	if im.URL != "" || im.Auth != "" || im.OAuth != nil || len(im.ToolsDefaultAllow) > 0 {
		return invalid("a native plugin has binaries, not an MCP server's url, auth or tools")
	}
	if len(im.Binaries) == 0 {
		return invalid("a native plugin needs a binary for at least one platform")
	}
	for platform, b := range im.Binaries {
		if !platformPattern.MatchString(platform) {
			return invalid("%q is not GOOS/GOARCH", platform)
		}
		if !sha256Pattern.MatchString(b.SHA256) {
			return invalid("%s: sha256 must be 64 lowercase hex digits", platform)
		}
		if err := checkDownloadURL(b.URL); err != nil {
			return fmt.Errorf("%s: %w", platform, err)
		}
	}
	return nil
}

// CheckServerURL holds an address the server will send credentials to (see mcpremote.CheckURL).
func CheckServerURL(raw string) error {
	if err := mcpremote.CheckURL(raw); err != nil {
		return invalid("%v", err)
	}
	return nil
}

// checkDownloadURL holds where a binary comes from: https, http to localhost, or a file on the
// server. The checksum is what makes it safe; this keeps it from being read in the clear.
func checkDownloadURL(raw string) error {
	if strings.HasPrefix(raw, "file://") {
		return nil
	}
	return CheckServerURL(raw)
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}
