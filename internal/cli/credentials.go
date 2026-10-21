package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// ErrNoToken means nothing is stored for the server.
var ErrNoToken = errors.New("no stored token")

// Store keeps one token per server.
type Store interface {
	Get(server string) (string, error)
	Set(server, token string) error
	Delete(server string) error
	// Where says where the token lives, for the person reading `gator login`'s answer.
	Where() string
}

// keychainService is the CLI's own entry, apart from Gator.app's (dev.onegator.gator): each
// holds its own token, so signing out of one leaves the other signed in, and either can be
// revoked from the list of tokens on its own.
const keychainService = "dev.onegator.gator.cli"

// DefaultStore is the macOS login keychain where there is one, else a 0600 file beside the
// config.
func DefaultStore(dir string) Store {
	if runtime.GOOS == "darwin" {
		if path, err := exec.LookPath("security"); err == nil {
			return keychain{security: path}
		}
	}
	return FileStore{Path: filepath.Join(dir, "credentials.json")}
}

// keychain drives /usr/bin/security rather than linking Security.framework: the binaries are
// built with CGO_ENABLED=0 for every platform, and the tool is on every Mac.
type keychain struct{ security string }

func (keychain) Where() string { return "the macOS keychain (" + keychainService + ")" }

func (k keychain) Get(server string) (string, error) {
	out, err := exec.Command(k.security, "find-generic-password", "-s", keychainService, "-a", server, "-w").Output()
	if err != nil {
		var exit *exec.ExitError
		// 44 is errSecItemNotFound.
		if errors.As(err, &exit) && exit.ExitCode() == 44 {
			return "", ErrNoToken
		}
		return "", fmt.Errorf("reading the keychain: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// tokenShape is what the server hands out. Anything else is refused before it reaches the
// security command line, where it would be parsed.
var tokenShape = regexp.MustCompile(`^[A-Za-z0-9_\-.]+$`)

// Set feeds the command to `security -i` on stdin, so the token never appears in the process
// list the way `-w TOKEN` on the command line would.
func (k keychain) Set(server, token string) error {
	if !tokenShape.MatchString(token) {
		return coded(exitUserErr, "that does not look like a Gator token")
	}
	if strings.ContainsAny(server, "\"\\\n") {
		return coded(exitUserErr, "server address has characters the keychain command cannot carry")
	}
	cmd := exec.Command(k.security, "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %q -a %q -l %q -w %q\n",
		keychainService, server, "Gator CLI ("+server+")", token))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("writing the keychain: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	// security -i exits 0 even when a command inside it failed, so read the entry back.
	got, err := k.Get(server)
	if err != nil || got != token {
		return fmt.Errorf("the keychain did not keep the token: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (k keychain) Delete(server string) error {
	err := exec.Command(k.security, "delete-generic-password", "-s", keychainService, "-a", server).Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 44 {
		return ErrNoToken
	}
	return err
}

// FileStore keeps tokens in a JSON file readable by its owner only, for machines without a
// keychain (Linux servers, CI).
type FileStore struct{ Path string }

func (f FileStore) Where() string { return f.Path }

func (f FileStore) read() (map[string]string, error) {
	tokens := map[string]string{}
	raw, err := os.ReadFile(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return tokens, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", f.Path, err)
	}
	return tokens, nil
}

func (f FileStore) write(tokens map[string]string) error {
	raw, _ := json.MarshalIndent(tokens, "", "  ")
	return writePrivate(f.Path, append(raw, '\n'))
}

func (f FileStore) Get(server string) (string, error) {
	tokens, err := f.read()
	if err != nil {
		return "", err
	}
	if t, ok := tokens[server]; ok && t != "" {
		return t, nil
	}
	return "", ErrNoToken
}

func (f FileStore) Set(server, token string) error {
	tokens, err := f.read()
	if err != nil {
		return err
	}
	tokens[server] = token
	return f.write(tokens)
}

func (f FileStore) Delete(server string) error {
	tokens, err := f.read()
	if err != nil {
		return err
	}
	if _, ok := tokens[server]; !ok {
		return ErrNoToken
	}
	delete(tokens, server)
	return f.write(tokens)
}
