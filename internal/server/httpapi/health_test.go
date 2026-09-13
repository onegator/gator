package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeDB struct{ err error }

func (f fakeDB) Ready(context.Context) error { return f.err }

func TestHealthz(t *testing.T) {
	srv := httptest.NewServer(NewRouter(fakeDB{}))
	defer srv.Close()
	res, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d", res.StatusCode)
	}
}

func TestReadyzReportsDatabase(t *testing.T) {
	srv := httptest.NewServer(NewRouter(fakeDB{err: errors.New("down")}))
	defer srv.Close()
	res, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz with db down: %d", res.StatusCode)
	}
}
