package api

import (
	"net/http"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// RegisterDevice records the caller's device for push notifications.
func (s *Server) RegisterDevice(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	if p.Kind != auth.KindUser || !p.UserID.Valid {
		writeError(w, http.StatusForbidden, "only a signed-in person registers a device", "forbidden")
		return
	}
	var in gen.NewDevice
	if !decode(w, r, &in) {
		return
	}
	if in.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required", "invalid")
		return
	}
	device, err := db.New(s.Pool).UpsertDevice(r.Context(), db.UpsertDeviceParams{
		UserID: p.UserID, Token: in.Token, Platform: string(in.Platform), AppVersion: deref(in.AppVersion),
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	out := gen.Device{Id: toUUID(device.ID), Platform: device.Platform, CreatedAt: device.CreatedAt.Time}
	if device.AppVersion != "" {
		out.AppVersion = &device.AppVersion
	}
	writeJSON(w, http.StatusOK, out)
}

// ForgetDevice stops notifications to one of the caller's devices.
func (s *Server) ForgetDevice(w http.ResponseWriter, r *http.Request, deviceToken string) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	n, err := db.New(s.Pool).DeleteDevice(r.Context(), db.DeleteDeviceParams{Token: deviceToken, UserID: p.UserID})
	if err != nil {
		s.fail(w, err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "no such device", "not_found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
