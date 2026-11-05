package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
)

func (s *Server) taskContextUsage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid task id")
		return
	}
	parse := func(name string, defaultValue int) (int, bool) {
		value := r.URL.Query().Get(name)
		if value == "" {
			return defaultValue, true
		}
		n, e := strconv.Atoi(value)
		return n, e == nil
	}
	offset, ok := parse("offset", 0)
	if !ok || offset < 0 || offset > 1000000 {
		writeErr(w, 400, "invalid offset")
		return
	}
	limit, ok := parse("limit", 50)
	if !ok || limit < 1 || limit > 200 {
		writeErr(w, 400, "limit must be 1..200")
		return
	}
	out, err := s.m.pg.TaskContextUsage(r.Context(), id, offset, limit)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 404, "task not found")
		return
	}
	if err != nil {
		writeErr(w, 500, "context usage unavailable")
		return
	}
	writeJSON(w, 200, out)
}
