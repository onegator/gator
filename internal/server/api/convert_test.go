package api

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// The task detail is the same task with its gate and phases, but it is assembled by listing
// fields by hand — and twice now a field was added to a task and forgotten here, so the app
// showed an empty space where the description, and then where the origin, should have been.
// Everything a task carries must arrive in its detail.
func TestTheDetailCarriesEverythingTheTaskDoes(t *testing.T) {
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	task := db.Task{
		ID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true}, ProjectID: pgtype.UUID{Bytes: [16]byte{2}, Valid: true},
		Kind: "bug", Title: "Login is broken", Description: "It fails after a password reset.",
		Phase: "planning", Urgency: 2, RequirementsChanged: true,
		Origin: "external", OriginSource: "github", AdmittedAt: now,
		PhaseEnteredAt: now, CreatedAt: now,
	}
	simple := toTask(task)
	detail := toDetail(process.Detail{Task: task, Phase: process.Phase{Name: "planning"}})

	st, sv := reflect.TypeOf(simple), reflect.ValueOf(simple)
	dv := reflect.ValueOf(detail)
	for i := range st.NumField() {
		name := st.Field(i).Name
		got := dv.FieldByName(name)
		if !got.IsValid() {
			t.Errorf("the detail has no %s at all", name)
			continue
		}
		// Enum fields are generated as a type per schema, so compare what they say.
		if text(got) != text(sv.Field(i)) {
			t.Errorf("%s: detail has %v, task has %v", name, text(got), text(sv.Field(i)))
		}
	}
}

// text renders a field for comparison, following pointers and reading enums as their strings.
func text(v reflect.Value) string {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "<nil>"
		}
		v = v.Elem()
	}
	if v.Kind() == reflect.String {
		return v.String()
	}
	return fmt.Sprint(v.Interface())
}
