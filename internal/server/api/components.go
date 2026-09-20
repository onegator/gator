package api

import (
	"fmt"
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// ApproveComponent accepts a component the server worked out from where jobs change code. Until
// somebody does, it is a suggestion on a screen and reaches no prompt. Rejecting one is
// deleting it, which is the same gesture as removing any other component.
func (s *Server) ApproveComponent(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, key string) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleAdmin); !ok {
		return
	}
	updated, err := db.New(s.Pool).SetComponentStatus(r.Context(), db.SetComponentStatusParams{
		ProjectID: fromUUID(projectId), Key: key, Status: "approved"})
	if err != nil {
		s.fail(w, err)
		return
	}
	out, err := s.components(r, fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, c := range out {
		if c.Key == updated.Key {
			writeJSON(w, http.StatusOK, c)
			return
		}
	}
	writeError(w, http.StatusNotFound, "no such component", "not_found")
}

// ListComponents returns the catalogue with each component's neighbours and decisions, so the
// screen and the prompt agree on what a component is.
func (s *Server) ListComponents(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer); !ok {
		return
	}
	out, err := s.components(r, fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) components(r *http.Request, projectID pgtype.UUID) ([]gen.Component, error) {
	ctx := r.Context()
	q := db.New(s.Pool)
	rows, err := q.ListComponents(ctx, projectID)
	if err != nil {
		return nil, err
	}
	ids := make([]pgtype.UUID, 0, len(rows))
	for _, c := range rows {
		ids = append(ids, c.ID)
	}
	deps, err := q.ListComponentDeps(ctx, ids)
	if err != nil {
		return nil, err
	}
	dependents, err := q.ListComponentDependents(ctx, ids)
	if err != nil {
		return nil, err
	}
	decisions, err := q.ListComponentDecisions(ctx, ids)
	if err != nil {
		return nil, err
	}
	dependsOn := map[[16]byte][]string{}
	for _, d := range deps {
		dependsOn[d.OfID.Bytes] = append(dependsOn[d.OfID.Bytes], d.Component.Key)
	}
	usedBy := map[[16]byte][]string{}
	for _, d := range dependents {
		usedBy[d.OfID.Bytes] = append(usedBy[d.OfID.Bytes], d.Component.Key)
	}
	linked := map[[16]byte][]gen.ProductEntry{}
	for _, d := range decisions {
		linked[d.OfID.Bytes] = append(linked[d.OfID.Bytes], toProductEntry(d.ProductContext))
	}
	out := make([]gen.Component, 0, len(rows))
	for _, c := range rows {
		item := gen.Component{
			Id: toUUID(c.ID), Key: c.Key, Name: c.Name, Kind: gen.ComponentKind(c.Kind),
			Repo: &c.Repo, Path: &c.Path, Notes: &c.Notes,
			DependsOn: orEmptyKeys(dependsOn[c.ID.Bytes]), UsedBy: orEmptyKeys(usedBy[c.ID.Bytes]),
			Decisions: linked[c.ID.Bytes],
		}
		status := gen.ComponentStatus(c.Status)
		item.Status, item.ProposedReason = &status, &c.ProposedReason
		if item.Decisions == nil {
			item.Decisions = []gen.ProductEntry{}
		}
		if c.OwnerID.Valid {
			item.OwnerId = toUUIDPtr(c.OwnerID)
			if u, err := q.GetUser(ctx, c.OwnerID); err == nil {
				item.OwnerName = &u.Name
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// PutComponent adds a component or updates it by key. Dependencies and decisions are replaced
// wholesale: the body says what the component is now, not what to add to it.
func (s *Server) PutComponent(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleAdmin); !ok {
		return
	}
	var in gen.ComponentInput
	if !decode(w, r, &in) {
		return
	}
	projectID := fromUUID(projectId)
	ctx := r.Context()
	q := db.New(s.Pool)
	c, err := q.UpsertComponent(ctx, db.UpsertComponentParams{
		ProjectID: projectID, Key: in.Key, Name: in.Name, Kind: string(in.Kind),
		Repo: deref(in.Repo), Path: deref(in.Path), Notes: deref(in.Notes),
		OwnerID: optionalUUID(in.OwnerId),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "component: "+err.Error(), "invalid")
		return
	}
	if in.DependsOn != nil {
		if err := s.setDeps(r, c, *in.DependsOn); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid")
			return
		}
	}
	if in.Decisions != nil {
		if err := s.setDecisions(r, c, *in.Decisions); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid")
			return
		}
	}
	out, err := s.components(r, projectID)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, item := range out {
		if item.Key == c.Key {
			writeJSON(w, http.StatusOK, item)
			return
		}
	}
	writeError(w, http.StatusNotFound, "not found", "not_found")
}

// optionalUUID turns an absent id into the null a nullable column wants.
func optionalUUID(id *openapi_types.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return fromUUID(*id)
}

// setDeps replaces a component's dependencies. A key from another project simply does not
// resolve here, which is how the catalogue stays inside one project.
func (s *Server) setDeps(r *http.Request, c db.Component, keys []string) error {
	ctx := r.Context()
	q := db.New(s.Pool)
	current, err := q.ListComponentDeps(ctx, []pgtype.UUID{c.ID})
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, k := range keys {
		if k == c.Key {
			return fmt.Errorf("%s cannot depend on itself", c.Key)
		}
		want[k] = true
	}
	for _, d := range current {
		if !want[d.Component.Key] {
			if _, err := q.RemoveComponentDep(ctx, db.RemoveComponentDepParams{ComponentID: c.ID, DependsOnID: d.Component.ID}); err != nil {
				return err
			}
		}
		delete(want, d.Component.Key)
	}
	for key := range want {
		other, err := q.GetComponentByKey(ctx, db.GetComponentByKeyParams{ProjectID: c.ProjectID, Key: key})
		if err != nil {
			return fmt.Errorf("no component %q in this project", key)
		}
		if err := q.SetComponentDep(ctx, db.SetComponentDepParams{ComponentID: c.ID, DependsOnID: other.ID}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) setDecisions(r *http.Request, c db.Component, ids []openapi_types.UUID) error {
	ctx := r.Context()
	q := db.New(s.Pool)
	current, err := q.ListComponentDecisions(ctx, []pgtype.UUID{c.ID})
	if err != nil {
		return err
	}
	want := map[[16]byte]pgtype.UUID{}
	for _, id := range ids {
		want[fromUUID(id).Bytes] = fromUUID(id)
	}
	for _, d := range current {
		if _, ok := want[d.ProductContext.ID.Bytes]; !ok {
			if _, err := q.UnlinkComponentDecision(ctx, db.UnlinkComponentDecisionParams{ComponentID: c.ID, EntryID: d.ProductContext.ID}); err != nil {
				return err
			}
		}
		delete(want, d.ProductContext.ID.Bytes)
	}
	for _, id := range want {
		if err := q.LinkComponentDecision(ctx, db.LinkComponentDecisionParams{ComponentID: c.ID, EntryID: id}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) DeleteComponent(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, key string) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleAdmin); !ok {
		return
	}
	q := db.New(s.Pool)
	c, err := q.GetComponentByKey(r.Context(), db.GetComponentByKeyParams{ProjectID: fromUUID(projectId), Key: key})
	if err != nil {
		writeError(w, http.StatusNotFound, "not found", "not_found")
		return
	}
	n, err := q.DeleteComponent(r.Context(), db.DeleteComponentParams{ID: c.ID, ProjectID: fromUUID(projectId)})
	if err != nil {
		s.fail(w, err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not found", "not_found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetTaskComponent says which part of the product a task is about. The job's prompt then
// carries that part and its neighbours instead of the whole catalogue.
func (s *Server) SetTaskComponent(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleMember); !ok {
		return
	}
	var in gen.TaskComponent
	if !decode(w, r, &in) {
		return
	}
	q := db.New(s.Pool)
	t, err := q.GetTask(r.Context(), fromUUID(taskId))
	if err != nil {
		s.fail(w, err)
		return
	}
	var componentID pgtype.UUID
	if in.Key != nil && *in.Key != "" {
		c, err := q.GetComponentByKey(r.Context(), db.GetComponentByKeyParams{ProjectID: t.ProjectID, Key: *in.Key})
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("no component %q in this project", *in.Key), "invalid")
			return
		}
		componentID = c.ID
	}
	updated, err := q.SetTaskComponent(r.Context(), db.SetTaskComponentParams{ID: t.ID, ComponentID: componentID})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTask(updated))
}

func orEmptyKeys(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
