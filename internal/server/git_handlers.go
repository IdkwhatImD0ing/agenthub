package server

import (
	"io"
	"net/http"
	"os"
	"strconv"

	"agenthub/internal/auth"
	"agenthub/internal/db"
	"agenthub/internal/gitrepo"
)

func (s *Server) handleGitPush(w http.ResponseWriter, r *http.Request) {
	agent := auth.AgentFromContext(r.Context())

	// Rate limit check
	allowed, err := s.db.CheckRateLimit(agent.ID, "push", s.config.MaxPushesPerHour)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "rate limit check failed")
		return
	}
	if !allowed {
		writeError(w, http.StatusTooManyRequests, "push rate limit exceeded")
		return
	}

	// Read bundle with size limit
	r.Body = http.MaxBytesReader(w, r.Body, s.config.MaxBundleSize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "bundle too large")
		return
	}

	// Write to temp file
	tmpFile, err := os.CreateTemp("", "arhub-push-*.bundle")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create temp file")
		return
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(body); err != nil {
		tmpFile.Close()
		writeError(w, http.StatusInternalServerError, "failed to write bundle")
		return
	}
	tmpFile.Close()

	// Unbundle into bare repo
	hashes, err := s.repo.Unbundle(tmpFile.Name())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid bundle: "+err.Error())
		return
	}

	// Index each new commit in the database
	var indexed []string
	for _, hash := range hashes {
		// Skip if already indexed
		existing, _ := s.db.GetCommit(hash)
		if existing != nil {
			indexed = append(indexed, hash)
			continue
		}

		parentHash, message, err := s.repo.GetCommitInfo(hash)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read commit info")
			return
		}

		// Validate parent exists (unless root commit)
		if parentHash != "" && !s.repo.CommitExists(parentHash) {
			writeError(w, http.StatusBadRequest, "parent commit not found: "+parentHash)
			return
		}

		// Also index the parent if it's not in DB yet (e.g. seed repo commits)
		if parentHash != "" {
			if pc, _ := s.db.GetCommit(parentHash); pc == nil {
				pParent, pMsg, _ := s.repo.GetCommitInfo(parentHash)
				s.db.InsertCommit(parentHash, pParent, "", pMsg)
			}
		}

		if err := s.db.InsertCommit(hash, parentHash, agent.ID, message); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to index commit")
			return
		}
		indexed = append(indexed, hash)
	}

	// Increment rate limit
	s.db.IncrementRateLimit(agent.ID, "push")

	writeJSON(w, http.StatusCreated, map[string]any{
		"hashes": indexed,
	})
}

func (s *Server) handleGitFetch(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !gitrepo.IsValidHash(hash) {
		writeError(w, http.StatusBadRequest, "invalid hash")
		return
	}

	if !s.repo.CommitExists(hash) {
		writeError(w, http.StatusNotFound, "commit not found")
		return
	}

	bundlePath, err := s.repo.CreateBundle(hash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create bundle")
		return
	}
	defer os.Remove(bundlePath)

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename="+hash+".bundle")
	http.ServeFile(w, r, bundlePath)
}

func (s *Server) handleListCommits(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	commits, err := s.db.ListCommits(agentID, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if commits == nil {
		commits = []db.Commit{}
	}
	writeJSON(w, http.StatusOK, commits)
}

func (s *Server) handleGetCommit(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !gitrepo.IsValidHash(hash) {
		writeError(w, http.StatusBadRequest, "invalid hash")
		return
	}

	commit, err := s.db.GetCommit(hash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if commit == nil {
		writeError(w, http.StatusNotFound, "commit not found")
		return
	}
	writeJSON(w, http.StatusOK, commit)
}

func (s *Server) handleGetChildren(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !gitrepo.IsValidHash(hash) {
		writeError(w, http.StatusBadRequest, "invalid hash")
		return
	}

	children, err := s.db.GetChildren(hash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if children == nil {
		children = []db.Commit{}
	}
	writeJSON(w, http.StatusOK, children)
}

func (s *Server) handleGetLineage(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !gitrepo.IsValidHash(hash) {
		writeError(w, http.StatusBadRequest, "invalid hash")
		return
	}

	lineage, err := s.db.GetLineage(hash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if lineage == nil {
		lineage = []db.Commit{}
	}
	writeJSON(w, http.StatusOK, lineage)
}

func (s *Server) handleGetLeaves(w http.ResponseWriter, r *http.Request) {
	leaves, err := s.db.GetLeaves()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if leaves == nil {
		leaves = []db.Commit{}
	}
	writeJSON(w, http.StatusOK, leaves)
}

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	agent := auth.AgentFromContext(r.Context())
	// Rate limit diffs (CPU-expensive)
	allowed, _ := s.db.CheckRateLimit(agent.ID, "diff", 60)
	if !allowed {
		writeError(w, http.StatusTooManyRequests, "diff rate limit exceeded")
		return
	}

	hashA := r.PathValue("hash_a")
	hashB := r.PathValue("hash_b")
	if !gitrepo.IsValidHash(hashA) || !gitrepo.IsValidHash(hashB) {
		writeError(w, http.StatusBadRequest, "invalid hash")
		return
	}

	diff, err := s.repo.Diff(hashA, hashB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "diff failed")
		return
	}

	s.db.IncrementRateLimit(agent.ID, "diff")
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(diff))
}
