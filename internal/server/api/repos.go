package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

var (
	repoName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	scpLike  = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^\s]+$`)
	branch   = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	// owner/repo, optionally after github.com/: what people copy from a GitHub page.
	githubShorthand = regexp.MustCompile(`^(?:github\.com/)?([A-Za-z0-9-]{1,39})/([A-Za-z0-9._-]{1,100}?)(?:\.git)?/?$`)
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

// normalizeRepoURL turns GitHub shorthand into the address git clones; anything else is
// left as typed.
func normalizeRepoURL(u string) string {
	u = strings.TrimSpace(u)
	if m := githubShorthand.FindStringSubmatch(u); m != nil {
		return "https://github.com/" + m[1] + "/" + m[2] + ".git"
	}
	return u
}

type repoInput struct {
	name, url, branch string
}

var errInvalidRepo = errors.New("invalid repository name, url or default branch")

func parseRepo(name, url string, defaultBranch *string) (repoInput, error) {
	def := "main"
	if defaultBranch != nil && strings.TrimSpace(*defaultBranch) != "" {
		def = strings.TrimSpace(*defaultBranch)
	}
	in := repoInput{name: name, url: normalizeRepoURL(url), branch: def}
	if !repoName.MatchString(in.name) || !validRepoURL(in.url) || !branch.MatchString(def) || strings.HasPrefix(def, "-") {
		return repoInput{}, errInvalidRepo
	}
	return in, nil
}

// putRepo adds or updates a repository. The first one a project gets is primary whatever was
// asked, because it is the one runners check out.
func (s *Server) putRepo(ctx context.Context, project pgtype.UUID, in repoInput, primary bool) (db.ProjectRepo, error) {
	q := db.New(s.Pool)
	existing, err := q.ListProjectRepos(ctx, project)
	if err != nil {
		return db.ProjectRepo{}, err
	}
	if len(existing) == 0 || (len(existing) == 1 && existing[0].Name == in.name) {
		primary = true
	}
	rp, err := q.UpsertProjectRepo(ctx, db.UpsertProjectRepoParams{ProjectID: project, Name: in.name, Url: in.url, DefaultBranch: in.branch, IsPrimary: primary})
	if err != nil {
		return db.ProjectRepo{}, err
	}
	if primary {
		if err := q.ClearOtherPrimaryRepos(ctx, db.ClearOtherPrimaryReposParams{ProjectID: project, Name: in.name}); err != nil {
			return db.ProjectRepo{}, err
		}
	}
	return rp, nil
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
	writeJSON(w, http.StatusOK, mapAll(rows, toRepo))
}

func (s *Server) PutProjectRepo(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleAdmin); !ok {
		return
	}
	var in gen.ProjectRepo
	if !decode(w, r, &in) {
		return
	}
	repo, err := parseRepo(in.Name, in.Url, in.DefaultBranch)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid")
		return
	}
	rp, err := s.putRepo(r.Context(), fromUUID(projectId), repo, in.Primary != nil && *in.Primary)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRepo(rp))
}

// DeleteProjectRepo forgets a repository. Taking the primary one away while others remain
// would leave runners to guess which to check out, so another must be made primary first.
func (s *Server) DeleteProjectRepo(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, name string) {
	project := fromUUID(projectId)
	if _, ok := s.requireProject(w, r, project, auth.RoleAdmin); !ok {
		return
	}
	q := db.New(s.Pool)
	rp, err := q.GetProjectRepo(r.Context(), db.GetProjectRepoParams{ProjectID: project, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "this project has no repository called "+name, "not_found")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if rp.IsPrimary {
		all, err := q.ListProjectRepos(r.Context(), project)
		if err != nil {
			s.fail(w, err)
			return
		}
		if len(all) > 1 {
			writeError(w, http.StatusConflict, "this is the primary repository; make another one primary first", "conflict")
			return
		}
	}
	if err := q.DeleteProjectRepo(r.Context(), db.DeleteProjectRepoParams{ProjectID: project, Name: name}); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func toRepo(r db.ProjectRepo) gen.ProjectRepo {
	def, primary := r.DefaultBranch, r.IsPrimary
	return gen.ProjectRepo{Name: r.Name, Url: r.Url, DefaultBranch: &def, Primary: &primary}
}
