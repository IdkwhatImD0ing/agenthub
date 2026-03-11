package server

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
)

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	// Check if agent already exists
	existing, err := s.db.GetAgentByID(req.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if existing != nil {
		writeError(w, http.StatusConflict, "agent already exists")
		return
	}

	// Generate random API key
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate api key")
		return
	}
	apiKey := hex.EncodeToString(keyBytes)

	if err := s.db.CreateAgent(req.ID, apiKey); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create agent")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"id":      req.ID,
		"api_key": apiKey,
	})
}

var agentIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

// handleRegister is the public self-registration endpoint (no admin key needed).
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	// Rate limit by IP (10 registrations per hour per IP)
	ip := strings.Split(r.RemoteAddr, ":")[0]
	allowed, err := s.db.CheckRateLimit("ip:"+ip, "register", 10)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "rate limit check failed")
		return
	}
	if !allowed {
		writeError(w, http.StatusTooManyRequests, "registration rate limit exceeded")
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !agentIDRe.MatchString(req.ID) {
		writeError(w, http.StatusBadRequest, "id must be 1-63 chars, alphanumeric/dash/dot/underscore, start with alphanumeric")
		return
	}

	existing, err := s.db.GetAgentByID(req.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if existing != nil {
		writeError(w, http.StatusConflict, "agent id already taken")
		return
	}

	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate api key")
		return
	}
	apiKey := hex.EncodeToString(keyBytes)

	if err := s.db.CreateAgent(req.ID, apiKey); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create agent")
		return
	}

	s.db.IncrementRateLimit("ip:"+ip, "register")

	writeJSON(w, http.StatusCreated, map[string]string{
		"id":      req.ID,
		"api_key": apiKey,
	})
}
