package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/marketplace/marketplacetest"
)

// nativePackage is a native plugin's integration package: the echo test plugin, built for this
// test into a temp dir (nothing is committed), served from a fake download server.
func nativePackage(t *testing.T, sha string) (name string, files map[string]string, served *int) {
	t.Helper()
	bin, err := os.ReadFile(echoPlugin(t))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		_, _ = w.Write(bin)
	}))
	t.Cleanup(srv.Close)
	if sha == "" {
		sum := sha256.Sum256(bin)
		sha = hex.EncodeToString(sum[:])
	}
	name = fmt.Sprintf("echo-m%d", time.Now().UnixNano())
	return name, map[string]string{"integration.yaml": `name: ` + name + `
version: 1.0.0
display_name: Echo
description: Says what it hears.
kind: native
capabilities: [ci]
binaries:
  ` + runtime.GOOS + `/` + runtime.GOARCH + `: {url: "` + srv.URL + `/build", sha256: "` + sha + `"}
  plan9/arm: {url: "` + srv.URL + `/plan9", sha256: "` + strings.Repeat("0", 64) + `"}
`}, &n
}

// A native plugin comes from the registry as a build per platform: this server's is downloaded,
// checked against the sha256 the (signed) index vouches for, made executable only then, and
// installed for the workspace, where each project switches it on. A build that does not match is
// never made executable.
func TestANativePluginIsCheckedBeforeItRuns(t *testing.T) {
	h := newHarness(t)
	name, files, _ := nativePackage(t, "")
	r := marketplacetest.New(t)
	r.Package("integrations/"+name, files)
	bad, badFiles, _ := nativePackage(t, strings.Repeat("ab", 32))
	r.Package("integrations/"+bad, badFiles)
	r.Publish()
	reg, _ := h.registry(r)
	p := h.oneProject("native")

	in := gen.NewInstallation{Kind: "integration", Name: name, RegistryId: &reg.Id, AcceptedPermissions: h.accept("integration", name, reg.Id.String())}
	if code := h.do("POST", "/projects/"+p.Id.String()+"/installations", in, nil); code != 400 {
		t.Fatalf("a native plugin is the workspace's: %d", code)
	}
	if code := h.asJob(h.person(), "POST", "/workspace/installations", in, nil); code != 403 {
		t.Fatalf("a person who is not a workspace admin: %d", code)
	}
	var inst gen.Installation
	if code := h.do("POST", "/workspace/installations", in, &inst); code != 201 {
		t.Fatalf("install: %d", code)
	}
	var installed []gen.InstalledPlugin
	h.do("GET", "/plugins", nil, &installed)
	var cmd string
	for _, ip := range installed {
		if ip.Name == name {
			cmd = ip.Command[0]
		}
	}
	info, err := os.Stat(cmd)
	if err != nil || info.Mode().Perm() != 0o755 || filepath.Base(cmd) != name || !strings.Contains(cmd, filepath.Join(name, "1.0.0")) {
		t.Fatalf("the build: %q %v %v", cmd, err, info)
	}
	// Nobody set it up yet: it goes, its build with it, and comes back.
	if code := h.do("DELETE", "/installations/"+inst.Id.String(), nil, nil); code != 204 {
		t.Fatalf("remove: %d", code)
	}
	if _, err := os.Stat(cmd); !os.IsNotExist(err) {
		t.Fatal("its build goes with it")
	}
	if code := h.do("POST", "/workspace/installations", in, &inst); code != 201 {
		t.Fatalf("install again: %d", code)
	}
	var v gen.ProjectPlugin
	if code := h.do("PUT", "/projects/"+p.Id.String()+"/plugins/"+name, gen.ProjectPluginSettings{Enabled: true, Config: &map[string]any{"greeting": "hi"}}, &v); code != 200 {
		t.Fatalf("a project switches it on: %d", code)
	}

	// The other build does not hash to what was promised: refused, and nothing of it can run.
	inBad := gen.NewInstallation{Kind: "integration", Name: bad, RegistryId: &reg.Id, AcceptedPermissions: h.accept("integration", bad, reg.Id.String())}
	var refusal gen.Error
	if code := h.do("POST", "/workspace/installations", inBad, &refusal); code != 400 || !strings.Contains(refusal.Error, "hashes to") {
		t.Fatalf("a build that does not match: %d %+v", code, refusal)
	}
	dir := filepath.Dir(filepath.Dir(filepath.Dir(cmd)))
	_ = filepath.Walk(filepath.Join(dir, bad), func(path string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			t.Errorf("a refused build was left behind: %s (%v)", path, fi.Mode())
		}
		return nil
	})
	h.do("GET", "/plugins", nil, &installed)
	for _, ip := range installed {
		if ip.Name == bad {
			t.Fatal("a refused build was installed")
		}
	}

	// A plugin a project has set up stays: its settings are the project's.
	if code := h.do("DELETE", "/installations/"+inst.Id.String(), nil, nil); code != 409 {
		t.Fatalf("removing a plugin a project has: %d", code)
	}
	if _, err := os.Stat(cmd); err != nil {
		t.Fatal("its build stays")
	}
}

// A native package cannot take the name of a plugin the server ships or one somebody installed:
// it would take over that plugin's command, and with it every project's secrets for it. A build
// that does not start as a plugin leaves nothing behind, and removing an installation never
// removes a plugin it did not put in place.
func TestANativePackageTakesNoOtherPluginsName(t *testing.T) {
	h := newHarness(t)
	if code := h.do("PUT", "/plugins/echo", gen.InstallPlugin{Command: []string{echoPlugin(t)}}, nil); code != 200 {
		t.Fatalf("install echo: %d", code)
	}
	r := marketplacetest.New(t)
	for _, name := range []string{"echo", h.bundled} {
		mine, files, _ := nativePackage(t, "")
		r.Package("integrations/"+name, map[string]string{"integration.yaml": strings.ReplaceAll(files["integration.yaml"], mine, name)})
	}
	// A build that is not a plugin at all.
	junk := []byte("#!/bin/sh\nexit 3\n")
	sum := sha256.Sum256(junk)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(junk) }))
	defer srv.Close()
	broken := fmt.Sprintf("echo-j%d", time.Now().UnixNano())
	r.Package("integrations/"+broken, map[string]string{"integration.yaml": "name: " + broken + "\nversion: 1.0.0\ndisplay_name: Junk\ndescription: Not a plugin.\nkind: native\nbinaries:\n  " +
		runtime.GOOS + "/" + runtime.GOARCH + `: {url: "` + srv.URL + `/junk", sha256: "` + hex.EncodeToString(sum[:]) + `"}` + "\n"})
	r.Publish()
	reg, _ := h.registry(r)

	var before []gen.InstalledPlugin
	h.do("GET", "/plugins", nil, &before)
	for _, name := range []string{"echo", h.bundled} {
		in := gen.NewInstallation{Kind: "integration", Name: name, RegistryId: &reg.Id, AcceptedPermissions: h.accept("integration", name, reg.Id.String())}
		var refusal gen.InstallRefusal
		if code := h.do("POST", "/workspace/installations", in, &refusal); code != 409 {
			t.Fatalf("a package named %s: %d %+v", name, code, refusal)
		}
	}
	var after []gen.InstalledPlugin
	h.do("GET", "/plugins", nil, &after)
	for _, p := range after {
		if p.Name == "echo" && p.Command[0] != echoPlugin(t) {
			t.Fatalf("echo's command was replaced: %v", p.Command)
		}
	}

	in := gen.NewInstallation{Kind: "integration", Name: broken, RegistryId: &reg.Id, AcceptedPermissions: h.accept("integration", broken, reg.Id.String())}
	if code := h.do("POST", "/workspace/installations", in, nil); code != 400 {
		t.Fatalf("a build that is not a plugin: %d", code)
	}
	if n := h.count(`SELECT count(*) FROM plugins WHERE name = $1`, broken); n != 0 {
		t.Fatal("the broken plugin's row stayed")
	}
	if n := h.count(`SELECT count(*) FROM installations WHERE name = $1`, broken); n != 0 {
		t.Fatal("an installation of nothing")
	}
}
