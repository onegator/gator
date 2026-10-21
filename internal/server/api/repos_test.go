package api

import "testing"

func TestValidRepoURL(t *testing.T) {
	ok := []string{"https://github.com/onegator/gator.git", "git@github.com:onegator/gator.git", "ssh://git@host/x.git", "file:///srv/repo.git", "/srv/repo.git"}
	bad := []string{"", "--upload-pack=touch /tmp/pwned", "-oProxyCommand=x", "ftp://x", "relative/path", "https://x y"}
	for _, u := range ok {
		if !validRepoURL(u) {
			t.Errorf("rejected %q", u)
		}
	}
	for _, u := range bad {
		if validRepoURL(u) {
			t.Errorf("accepted %q", u)
		}
	}
}

func TestNormalizeRepoURL(t *testing.T) {
	for in, want := range map[string]string{
		"onegator/gator":                    "https://github.com/onegator/gator.git",
		"github.com/onegator/gator":         "https://github.com/onegator/gator.git",
		" onegator/gator.git ":              "https://github.com/onegator/gator.git",
		"onegator/gator-app/":               "https://github.com/onegator/gator-app.git",
		"https://github.com/onegator/gator": "https://github.com/onegator/gator",
		"git@github.com:onegator/gator.git": "git@github.com:onegator/gator.git",
		"/srv/repo.git":                     "/srv/repo.git",
		"--upload-pack=touch /tmp/pwned":    "--upload-pack=touch /tmp/pwned",
	} {
		if got := normalizeRepoURL(in); got != want {
			t.Errorf("normalizeRepoURL(%q) = %q, want %q", in, got, want)
		}
	}
}
