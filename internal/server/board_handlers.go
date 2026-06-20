package server

import (
	"net/http"
	"regexp"
	"strconv"

	"agenthub/internal/auth"
	"agenthub/internal/db"
)

var channelNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,30}$`)

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	agent := auth.AgentFromContext(r.Context())
	proj, ok := s.requireProject(w, agent)
	if !ok {
		return
	}
	channels, err := s.db.ListChannels(proj.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if channels == nil {
		channels = []db.Channel{}
	}
	writeJSON(w, http.StatusOK, channels)
}

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	agent := auth.AgentFromContext(r.Context())
	if _, ok := s.requireOpenSession(w, agent); !ok {
		return
	}
	proj, ok := s.requireProject(w, agent)
	if !ok {
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !channelNameRe.MatchString(req.Name) {
		writeError(w, http.StatusBadRequest, "channel name must be 1-31 lowercase alphanumeric/dash/underscore chars")
		return
	}

	// Cap channels at 100 per project.
	channels, _ := s.db.ListChannels(proj.ID)
	if len(channels) >= 100 {
		writeError(w, http.StatusForbidden, "channel limit reached")
		return
	}

	// Check if channel already exists in this project
	existing, _ := s.db.GetChannelByName(proj.ID, req.Name)
	if existing != nil {
		writeError(w, http.StatusConflict, "channel already exists")
		return
	}

	if err := s.db.CreateChannel(proj.ID, req.Name, req.Description); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create channel")
		return
	}

	ch, _ := s.db.GetChannelByName(proj.ID, req.Name)
	writeJSON(w, http.StatusCreated, ch)
}

func (s *Server) handleListPosts(w http.ResponseWriter, r *http.Request) {
	agent := auth.AgentFromContext(r.Context())
	sessionID, ok := s.requireSession(w, agent)
	if !ok {
		return
	}
	proj, ok := s.requireProject(w, agent)
	if !ok {
		return
	}
	name := r.PathValue("name")
	ch, err := s.db.GetChannelByName(proj.ID, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "channel not found")
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	posts, err := s.db.ListPosts(ch.ID, sessionID, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if posts == nil {
		posts = []db.Post{}
	}
	writeJSON(w, http.StatusOK, posts)
}

func (s *Server) handleCreatePost(w http.ResponseWriter, r *http.Request) {
	agent := auth.AgentFromContext(r.Context())
	name := r.PathValue("name")

	sessionID, ok := s.requireOpenSession(w, agent)
	if !ok {
		return
	}
	proj, ok := s.requireProject(w, agent)
	if !ok {
		return
	}

	ch, err := s.db.GetChannelByName(proj.ID, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if ch == nil {
		// Auto-create on first post. Channels are cheap and project-scoped;
		// asking every worker to POST /api/channels before its first post
		// is pure boilerplate. Listing a missing channel still 404s — only
		// the post path implicitly creates.
		if err := s.db.CreateChannel(proj.ID, name, ""); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create channel")
			return
		}
		ch, _ = s.db.GetChannelByName(proj.ID, name)
		if ch == nil {
			writeError(w, http.StatusInternalServerError, "channel disappeared after create")
			return
		}
	}

	// Rate limit
	allowed, err := s.db.CheckRateLimit(agent.ID, "post", s.config.MaxPostsPerHour)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "rate limit check failed")
		return
	}
	if !allowed {
		writeError(w, http.StatusTooManyRequests, "post rate limit exceeded")
		return
	}

	var req struct {
		Content  string `json:"content"`
		ParentID *int   `json:"parent_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}
	if len(req.Content) > 32*1024 {
		writeError(w, http.StatusBadRequest, "post content too large (max 32KB)")
		return
	}

	// Validate parent post exists and belongs to same channel
	if req.ParentID != nil {
		parent, err := s.db.GetPost(*req.ParentID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		if parent == nil {
			writeError(w, http.StatusBadRequest, "parent post not found")
			return
		}
		if parent.ChannelID != ch.ID {
			writeError(w, http.StatusBadRequest, "parent post is in a different channel")
			return
		}
	}

	post, err := s.db.CreatePost(ch.ID, agent.ID, sessionID, req.ParentID, req.Content)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create post")
		return
	}

	s.db.IncrementRateLimit(agent.ID, "post")
	writeJSON(w, http.StatusCreated, post)
}

func (s *Server) handleGetPost(w http.ResponseWriter, r *http.Request) {
	agent := auth.AgentFromContext(r.Context())
	sessionID, ok := s.requireSession(w, agent)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid post id")
		return
	}

	post, err := s.db.GetPost(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	// 404 (not 403) for posts outside the caller's session so sequential
	// post ids cannot be used to probe other sessions' boards.
	if post == nil || post.SessionID != sessionID {
		writeError(w, http.StatusNotFound, "post not found")
		return
	}
	writeJSON(w, http.StatusOK, post)
}

func (s *Server) handleGetReplies(w http.ResponseWriter, r *http.Request) {
	agent := auth.AgentFromContext(r.Context())
	sessionID, ok := s.requireSession(w, agent)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid post id")
		return
	}

	// Verify post exists and is in the caller's session
	post, err := s.db.GetPost(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if post == nil || post.SessionID != sessionID {
		writeError(w, http.StatusNotFound, "post not found")
		return
	}

	replies, err := s.db.GetReplies(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if replies == nil {
		replies = []db.Post{}
	}
	writeJSON(w, http.StatusOK, replies)
}
