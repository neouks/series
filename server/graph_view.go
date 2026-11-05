package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/neouks/series/db"
)

func graphUint(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != raw {
		return 0, errors.New("invalid graph id or offset")
	}
	return n, nil
}

func graphReadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrGraphNodeUnavailable):
		writeErr(w, 404, "node not available in this task")
	case errors.Is(err, db.ErrGraphBodyChanged):
		writeErr(w, 409, err.Error())
	default:
		writeErr(w, 500, "graph read failed")
	}
}

func (s *Server) explorationGraphView(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.URL.Query().Get("task"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	q := r.URL.Query()
	focus, e1 := graphUint(q.Get("focus"))
	before, e2 := graphUint(q.Get("before"))
	upper, e3 := graphUint(q.Get("upper"))
	limit := int64(200)
	var e4 error
	if q.Has("limit") {
		limit, e4 = graphUint(q.Get("limit"))
	}
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || limit < 2 || limit > 400 || (before > 0 && upper == 0) || len(q.Get("revision")) > 64 {
		writeErr(w, 400, "invalid graph window")
		return
	}
	page, err := t.Store.GraphView(r.Context(), db.GraphViewQuery{Focus: focus, Before: before, Upper: upper, Limit: int(limit), Revision: q.Get("revision")})
	if err != nil {
		graphReadError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeGraphWindow(w, r, page, "")
}

func (s *Server) explorationGraphBody(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.URL.Query().Get("task"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	id, e1 := graphUint(r.PathValue("id"))
	offset, e2 := graphUint(r.URL.Query().Get("offset"))
	version := r.URL.Query().Get("body_version")
	if e1 != nil || e2 != nil || id == 0 || offset > 1<<30 || len(version) > 32 || (offset > 0 && version == "") {
		writeErr(w, 400, "invalid node body page")
		return
	}
	body, err := t.Store.GraphNodeBody(r.Context(), id, int(offset), version)
	if err != nil {
		graphReadError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, body)
}
