// Package marketplace is where a team gets agents, skills, integrations, processes and
// knowledge packs from: registries (git repositories, or directories) whose signed index names
// every package with a version and the checksum of its files. Fetching a registry verifies the
// signature and every package before anything is cached; installing one shows the permissions
// it asks for and installs only what a person accepted. Nothing updates by itself.
//
// The pieces are separate on purpose: a Fetcher brings a registry's files, a Verifier checks
// them, a Kind reads one kind of package and an Installer puts it into a project. A new kind is a
// new type in the list the module is built with.
package marketplace

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/onegator/gator/internal/names"
)

// Kinds of package.
const (
	KindAgent       = "agent"
	KindSkill       = "skill"
	KindIntegration = "integration"
	KindProcess     = "process"
	KindKnowledge   = "knowledge"
)

var (
	// ErrInvalid is a package, index or request that does not hold together.
	ErrInvalid = errors.New("invalid")
	// ErrSignature is an index whose signature does not verify against the registry's key.
	ErrSignature = errors.New("the registry's signature does not verify")
	// ErrChecksum is a package whose files do not hash to what its index promises, or a
	// published version whose files changed.
	ErrChecksum = errors.New("the package does not match its checksum")
	// ErrNotFound is a registry, package or installation that is not there for the caller.
	ErrNotFound = errors.New("not found")
	// ErrForbidden is a caller who may not do that.
	ErrForbidden = errors.New("forbidden")
	// ErrConflict is an installation that would replace something already there.
	ErrConflict = errors.New("conflict")
	// ErrPermissions is an install or update whose permissions a person has not accepted.
	ErrPermissions = errors.New("permissions not accepted")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// ValidName says whether s can name a package or a registry.
func ValidName(s string) bool { return names.Valid(s) }

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`)

// ValidVersion says whether s is a semantic version: 1.2.0, 2.0.0-rc.1.
func ValidVersion(s string) bool { return versionPattern.MatchString(s) }

// CompareVersions orders two valid versions as semver does, without build metadata: -1, 0 or 1.
// A pre-release is older than its release.
func CompareVersions(a, b string) int {
	ma, mb := versionPattern.FindStringSubmatch(a), versionPattern.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return strings.Compare(a, b)
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	pa, pb := strings.TrimPrefix(ma[4], "-"), strings.TrimPrefix(mb[4], "-")
	switch {
	case pa == pb:
		return 0
	case pa == "":
		return 1
	case pb == "":
		return -1
	}
	return comparePrerelease(pa, pb)
}

func comparePrerelease(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, errX := strconv.Atoi(as[i])
		y, errY := strconv.Atoi(bs[i])
		switch {
		case errX == nil && errY == nil && x != y:
			if x < y {
				return -1
			}
			return 1
		case errX == nil && errY != nil:
			return -1
		case errX != nil && errY == nil:
			return 1
		case errX != nil && errY != nil && as[i] != bs[i]:
			return strings.Compare(as[i], bs[i])
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}
