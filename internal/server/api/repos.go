package api

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

var (
	repoName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	scpLike  = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^\s]+$`)
	branch   = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
)

// validRepoURL accepts the forms git clones from. A leading "-" would be read by git as an
// option, so it is refused outright.
func validRepoURL(u string) bool {
	if u == "" || strings.HasPrefix(u, "-") || strings.ContainsAny(u, " \t\n") {
		return false
	}
	for _, p := range []string{"https://", "ssh://", "file://"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return strings.HasPrefix(u, "/") || scpLike.MatchString(u)
}

func (s *Server) ListProjectRepos(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer); !ok {
		return
	}
	rows, err := db.New(s.Pool).ListProjectRepos(r.Context(), fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.ProjectRepo, 0, len(rows))
	for _, rp := range rows {
		out = append(out, toRepo(rp))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) PutProjectRepo(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleAdmin); !ok {
		return
	}
	var in gen.ProjectRepo
	if !decode(w, r, &in) {
		return
	}
	def := "main"
	if in.DefaultBranch != nil && *in.DefaultBranch != "" {
		def = *in.DefaultBranch
	}
	if !repoName.MatchString(in.Name) || !validRepoURL(in.Url) || !branch.MatchString(def) || strings.HasPrefix(def, "-") {
		writeError(w, http.StatusBadRequest, "invalid repository name, url or default branch", "invalid")
		return
	}
	primary := in.Primary != nil && *in.Primary
	q := db.New(s.Pool)
	existing, err := q.ListProjectRepos(r.Context(), fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(existing) == 0 {
		primary = true // the first repository is the one runners use
	}
	rp, err := q.UpsertProjectRepo(r.Context(), db.UpsertProjectRepoParams{ProjectID: fromUUID(projectId), Name: in.Name, Url: in.Url, DefaultBranch: def, IsPrimary: primary})
	if err != nil {
		s.fail(w, err)
		return
	}
	if primary {
		if err := q.ClearOtherPrimaryRepos(r.Context(), db.ClearOtherPrimaryReposParams{ProjectID: fromUUID(projectId), Name: in.Name}); err != nil {
			s.fail(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, toRepo(rp))
}

func toRepo(r db.ProjectRepo) gen.ProjectRepo {
	def, primary := r.DefaultBranch, r.IsPrimary
	return gen.ProjectRepo{Name: r.Name, Url: r.Url, DefaultBranch: &def, Primary: &primary}
}
