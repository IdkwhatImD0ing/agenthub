package server

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"

	"agenthub/internal/auth"
	"agenthub/internal/db"
)

var sessionIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

// handleCreateSession (admin) opens a new session for a task. The operator owns
// session lifecycle; agents are later bound to the returned id.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Task string `json:"task"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Task == "" {
		writeError(w, http.StatusBadRequest, "task is required")
		return
	}
	if req.ID == "" {
		b := make([]byte, 8)
		rand.Read(b)
		req.ID = "s-" + hex.EncodeToString(b)
	}
	if !sessionIDRe.MatchString(req.ID) {
		writeError(w, http.StatusBadRequest, "id must be 1-63 chars, alphanumeric/dash/dot/underscore, start with alphanumeric")
		return
	}

	existing, err := s.db.GetSession(req.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if existing != nil {
		writeError(w, http.StatusConflict, "session already exists")
		return
	}

	if err := s.db.CreateSession(req.ID, req.Task); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create session")
		return
	}
	sess, _ := s.db.GetSession(req.ID)
	writeJSON(w, http.StatusCreated, sess)
}

// handleListSessions (admin) lists all sessions with activity counts.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.db.ListSessionStats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if sessions == nil {
		sessions = []db.SessionStats{}
	}
	writeJSON(w, http.StatusOK, sessions)
}

// handleCloseSession (admin) sets a terminal status and result. After this the
// session goes read-only and drops out of every agent's working set.
func (s *Server) handleCloseSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Status  string `json:"status"`
		Result  string `json:"result"`
		Commit  string `json:"commit"`
		Summary string `json:"summary"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Status == "" {
		req.Status = "done"
	}
	if req.Status != "done" && req.Status != "failed" {
		writeError(w, http.StatusBadRequest, "status must be 'done' or 'failed'")
		return
	}
	// Result is freeform; if commit/summary supplied, fold them in.
	result := req.Result
	if result == "" && (req.Commit != "" || req.Summary != "") {
		result = "commit=" + req.Commit + "\n" + req.Summary
	}

	if err := s.db.CloseSession(id, req.Status, result); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	sess, _ := s.db.GetSession(id)
	writeJSON(w, http.StatusOK, sess)
}

// handleGetCurrentSession returns the session the calling agent is bound to,
// so a freshly spawned agent can read its task.
func (s *Server) handleGetCurrentSession(w http.ResponseWriter, r *http.Request) {
	agent := auth.AgentFromContext(r.Context())
	if agent.SessionID == "" {
		writeError(w, http.StatusNotFound, "agent is not bound to a session")
		return
	}
	sess, err := s.db.GetSession(agent.SessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if sess == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// handleGetSession returns any session by id (read-only; archives stay visible).
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.db.GetSession(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if sess == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, sess)
}
