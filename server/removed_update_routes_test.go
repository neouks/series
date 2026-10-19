package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOnlineUpdateRoutesRemoved(t *testing.T) {
	// No manager, database, filesystem or network operations are needed to
	// verify the public router no longer exposes executable replacement.
	entry, err := newHTTPEntryAuth(func(context.Context) (httpEntryConfig, error) {
		return httpEntryConfig{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{jwtKey: []byte("update-removal-test-key"), httpAuth: entry}
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/update/check"},
		{"POST", "/api/update/apply"},
		{"POST", "/api/update/rollback"},
		{"GET", "/api/update/stream"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Header.Set("X-Series-Token", token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNotFound {
				t.Fatalf("removed route returned %d: %s", w.Code, w.Body.String())
			}
		})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/health", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("health route returned %d: %s", w.Code, w.Body.String())
	}
}
