package product

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/store/db"
)

// Before a directory is worth proposing as a component it has to have been worked in by this
// many separate jobs. One job passing through a file proves nothing; coming back does.
const minJobsForComponent = 2

// minFilesInDirectory keeps a single stray file from naming a component: a part of a product
// has more than one file in it.
const minFilesInDirectory = 2

// proposeComponents reads where a finished job actually changed code and keeps count. A
// directory no component covers, worked in more than once, becomes a proposed component — which
// a person approves or deletes, because the catalogue reaches every prompt and must not grow by
// itself.
func (c *Curator) proposeComponents(ctx context.Context, job db.Job) error {
	if len(job.Receipt) == 0 {
		return nil
	}
	var finish proto.Finish
	if err := json.Unmarshal(job.Receipt, &finish); err != nil || len(finish.ChangedPaths) == 0 {
		return nil //nolint:nilerr // a receipt we cannot read is not worth failing the drain for
	}
	q := db.New(c.pool)
	components, err := q.ListComponents(ctx, job.ProjectID)
	if err != nil {
		return err
	}
	covered := make([]string, 0, len(components))
	for _, comp := range components {
		if p := strings.Trim(comp.Path, "/"); p != "" {
			covered = append(covered, p)
		}
	}
	uncovered := uncoveredDirectories(finish.ChangedPaths, covered)
	for _, dir := range sortedKeys(uncovered) {
		hint, err := q.RecordComponentHint(ctx, db.RecordComponentHintParams{
			ProjectID: job.ProjectID, Path: dir, Files: int32(uncovered[dir]),
			LastJobID: pgtype.UUID{Bytes: job.ID.Bytes, Valid: true}})
		if err != nil {
			return err
		}
		if hint.ProposedAt.Valid || hint.Jobs < minJobsForComponent {
			continue
		}
		if err := c.proposeComponent(ctx, q, job, hint); err != nil {
			return err
		}
	}
	return nil
}

func (c *Curator) proposeComponent(ctx context.Context, q *db.Queries, job db.Job, hint db.ComponentHint) error {
	key := componentKey(hint.Path)
	reason := fmt.Sprintf("Jobs have changed %d files under %s in %d separate runs, and no component covers it.",
		hint.Files, hint.Path, hint.Jobs)
	_, err := q.ProposeComponent(ctx, db.ProposeComponentParams{
		ProjectID: job.ProjectID, Key: key, Name: componentName(hint.Path), Kind: "lib",
		Path: hint.Path, ProposedReason: reason,
	})
	// A key somebody already uses means the catalogue knows this part under another path;
	// marking the hint proposed stops it being suggested again every time.
	if err != nil && !isNoRows(err) {
		return err
	}
	if err := q.MarkHintProposed(ctx, hint.ID); err != nil {
		return err
	}
	if isNoRows(err) {
		return nil
	}
	c.log.Info("proposed a component from where jobs work", "project", uuidString(job.ProjectID),
		"key", key, "path", hint.Path)
	b, _ := json.Marshal(map[string]any{"key": key, "path": hint.Path, "reason": reason})
	_, err = q.InsertEvent(ctx, db.InsertEventParams{
		Type: "component.proposed", Aggregate: "project", AggregateID: job.ProjectID, Payload: b})
	return err
}

// uncoveredDirectories groups a job's changed files by the directory that would name them, and
// drops everything an existing component already covers. Files at the repository root name no
// component: "the repository" is not a part of a product.
func uncoveredDirectories(paths []string, covered []string) map[string]int {
	out := map[string]int{}
	for _, p := range paths {
		p = strings.TrimSpace(strings.Trim(p, "/"))
		if p == "" || coveredBy(p, covered) {
			continue
		}
		dir := componentDirectory(p)
		if dir == "" {
			continue
		}
		out[dir]++
	}
	for dir, files := range out {
		if files < minFilesInDirectory {
			delete(out, dir)
		}
	}
	return out
}

// componentDirectory is the part of a path that would name a component: the first two segments
// when there are enough of them, so "internal/server/plugins/host.go" suggests
// "internal/server" rather than every leaf directory in the tree.
func componentDirectory(p string) string {
	dir := path.Dir(p)
	if dir == "." || dir == "/" {
		return ""
	}
	parts := strings.Split(dir, "/")
	if len(parts) > 2 {
		parts = parts[:2]
	}
	return strings.Join(parts, "/")
}

// coveredBy says whether a component already claims this path.
func coveredBy(p string, covered []string) bool {
	for _, c := range covered {
		if p == c || strings.HasPrefix(p, c+"/") {
			return true
		}
	}
	return false
}

func componentKey(dir string) string {
	parts := strings.Split(dir, "/")
	return strings.ToLower(parts[len(parts)-1])
}

func componentName(dir string) string {
	key := componentKey(dir)
	if key == "" {
		return dir
	}
	return strings.ToUpper(key[:1]) + key[1:]
}

// isNoRows is the answer to an insert that conflicted: the catalogue already knows this key.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// sortedKeys keeps the order of proposals stable, which matters only so that a log of one run
// reads the same as the next.
func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
