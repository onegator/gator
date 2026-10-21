package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Config is what the CLI remembers between runs. The token is not here: it lives in the
// keychain (see Store), so this file can be read, copied or shown without leaking anything.
type Config struct {
	// Server is the one `gator login` signed in to.
	Server string `json:"server,omitempty"`
	// Project is the slug `gator use` chose, the default for --project.
	Project string `json:"project,omitempty"`
}

// ConfigDir is $XDG_CONFIG_HOME/gator, else ~/.config/gator — on macOS too, where a terminal
// user looks for dotfiles rather than in ~/Library/Application Support.
func ConfigDir(getenv func(string) string) string {
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "gator")
	}
	home := getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, ".config", "gator")
}

func configPath(dir string) string { return filepath.Join(dir, "config.json") }

// LoadConfig reads the config; a missing file is an empty config.
func LoadConfig(dir string) (Config, error) {
	var c Config
	raw, err := os.ReadFile(configPath(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, coded(exitUserErr, "%s is not valid JSON: %v", configPath(dir), err)
	}
	return c, nil
}

// SaveConfig writes the config atomically, readable by its owner only.
func SaveConfig(dir string, c Config) error {
	raw, _ := json.MarshalIndent(c, "", "  ")
	return writePrivate(configPath(dir), append(raw, '\n'))
}

// writePrivate replaces path with data through a temporary file, so a crash never leaves half
// a file, and keeps it 0600 in a 0700 directory.
func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// NormalizeServer turns what a person types into the base URL the CLI keys its token by:
// "gator.example.com" and "https://gator.example.com/" are the same server.
func NormalizeServer(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", coded(exitUserErr, "no server given")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", coded(exitUserErr, "%q is not a server address; use https://host[:port]", s)
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}
