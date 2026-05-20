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

// createSession performs the shared open-session work used by the JSON API and
// the dashboard form. Returns the new session, the HTTP status to use, and a
// human error message (empty when ok).
func (s *Server) createSession(id, task, base string) (*db.Session, int, string) {
	if task == "" {
		return nil, http.StatusBadRequest, "task is required"
	}
	if id == "" {
		b := make([]byte, 8)
		rand.Read(b)
		id = "s-" + hex.EncodeToString(b)
	}
	if !sessionIDRe.MatchString(id) {
		return nil, http.StatusBadRequest, "id must be 1-63 chars, alphanumeric/dash/dot/underscore, start with alphanumeric"
	}
	if existing, err := s.db.GetSession(id); err != nil {
		return nil, http.StatusInternalServerError, "database error"
	} else if existing != nil {
		return nil, http.StatusConflict, "session already exists"
	}

	// The snapshot baseline must be explicit. There is no global "current
	// repo" (the DAG has many tips across sessions), so defaulting would
	// silently pin an unrelated session's work. Without --base the session
	// starts empty and its first push becomes the root.
	if base != "" {
		if !s.repo.CommitExists(base) {
			return nil, http.StatusBadRequest, "base commit not found in hub"
		}
		if c, _ := s.db.GetCommit(base); c == nil {
			pHash, pMsg, _ := s.repo.GetCommitInfo(base)
			if err := s.db.InsertCommit(base, pHash, "", "", pMsg); err != nil {
				return nil, http.StatusInternalServerError, "failed to index snapshot"
			}
		}
		// Freeze the snapshot ref *before* persisting the session so a
		// session row never exists without its frozen baseline.
		if err := s.repo.CreateRef("refs/sessions/"+id, base); err != nil {
			return nil, http.StatusInternalServerError, "failed to freeze snapshot: " + err.Error()
		}
	}
	if err := s.db.CreateSession(id, task, base); err != nil {
		return nil, http.StatusInternalServerError, "failed to create session"
	}
	sess, _ := s.db.GetSession(id)
	return sess, http.StatusCreated, ""
}

// handleCreateSession (admin) opens a new session for a task. The operator owns
// session lifecycle; agents are later bound to the returned id.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Task string `json:"task"`
		Base string `json:"base"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	sess, status, errMsg := s.createSession(req.ID, req.Task, req.Base)
	if errMsg != "" {
		writeError(w, status, errMsg)
		return
	}
	writeJSON(w, status, sess)
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

// handleDeleteSession (admin) removes a session and all its posts, commits,
// agents, rate-limit counters, and the frozen snapshot ref.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.db.DeleteSession(id); err != nil {
		if err.Error() == "session not found" {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "delete failed: "+err.Error())
		return
	}
	// Best-effort: remove the snapshot ref. The DB row is already gone, so
	// failure here just leaves a dangling ref in the bare repo.
	s.repo.DeleteRef("refs/sessions/" + id)
	w.WriteHeader(http.StatusNoContent)
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
