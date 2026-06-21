package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"sync"

	"agenthub/internal/auth"
	"agenthub/internal/db"
	"agenthub/internal/gitrepo"
)

type Config struct {
	MaxBundleSize       int64  // max bundle upload size in bytes
	MaxPushesPerHour    int    // per agent
	MaxPostsPerHour     int    // per agent
	MaxAgentsPerSession int    // 0 = unlimited
	NoAuth              bool   // local mode: skip admin-key checks, open dashboard mutations
	ListenAddr          string // e.g. ":8080"
}

type Server struct {
	db       *db.DB
	dataDir  string                   // base data dir; per-project repos live under {dataDir}/projects/{slug}/repo.git
	repos    map[string]*gitrepo.Repo // slug -> bare repo, lazily initialized
	reposMu  sync.RWMutex
	adminKey string
	mux      *http.ServeMux
	config   Config
}

func New(database *db.DB, dataDir, adminKey string, cfg Config) *Server {
	s := &Server{
		db:       database,
		dataDir:  dataDir,
		repos:    make(map[string]*gitrepo.Repo),
		adminKey: adminKey,
		mux:      http.NewServeMux(),
		config:   cfg,
	}
	s.setupRoutes()
	return s
}

// getProjectRepo returns the bare git repo for a project, lazily creating it on
// disk under {dataDir}/projects/{slug}/repo.git and caching the handle.
func (s *Server) getProjectRepo(slug string) (*gitrepo.Repo, error) {
	s.reposMu.RLock()
	repo := s.repos[slug]
	s.reposMu.RUnlock()
	if repo != nil {
		return repo, nil
	}

	s.reposMu.Lock()
	defer s.reposMu.Unlock()
	// Re-check under the write lock in case another goroutine won the race.
	if repo := s.repos[slug]; repo != nil {
		return repo, nil
	}
	repo, err := gitrepo.Init(filepath.Join(s.dataDir, "projects", slug, "repo.git"))
	if err != nil {
		return nil, err
	}
	s.repos[slug] = repo
	return repo, nil
}

func (s *Server) setupRoutes() {
	authMw := auth.Middleware(s.db)
	adminMw := auth.AdminMiddleware(s.adminKey)
	if s.config.NoAuth {
		// Local mode: operator-only endpoints are open. Per-agent bearer auth
		// stays in place because the swarm still needs distinct identities.
		adminMw = func(h http.Handler) http.Handler { return h }
	}

	// Git endpoints
	s.mux.Handle("POST /api/git/push", authMw(http.HandlerFunc(s.handleGitPush)))
	s.mux.Handle("GET /api/git/fetch/{hash}", authMw(http.HandlerFunc(s.handleGitFetch)))
	s.mux.Handle("GET /api/git/commits", authMw(http.HandlerFunc(s.handleListCommits)))
	s.mux.Handle("GET /api/git/commits/{hash}", authMw(http.HandlerFunc(s.handleGetCommit)))
	s.mux.Handle("GET /api/git/commits/{hash}/children", authMw(http.HandlerFunc(s.handleGetChildren)))
	s.mux.Handle("GET /api/git/commits/{hash}/lineage", authMw(http.HandlerFunc(s.handleGetLineage)))
	s.mux.Handle("GET /api/git/leaves", authMw(http.HandlerFunc(s.handleGetLeaves)))
	s.mux.Handle("GET /api/git/diff/{hash_a}/{hash_b}", authMw(http.HandlerFunc(s.handleDiff)))

	// Message board endpoints
	s.mux.Handle("GET /api/channels", authMw(http.HandlerFunc(s.handleListChannels)))
	s.mux.Handle("POST /api/channels", authMw(http.HandlerFunc(s.handleCreateChannel)))
	s.mux.Handle("GET /api/channels/{name}/posts", authMw(http.HandlerFunc(s.handleListPosts)))
	s.mux.Handle("POST /api/channels/{name}/posts", authMw(http.HandlerFunc(s.handleCreatePost)))
	s.mux.Handle("GET /api/posts/{id}", authMw(http.HandlerFunc(s.handleGetPost)))
	s.mux.Handle("GET /api/posts/{id}/replies", authMw(http.HandlerFunc(s.handleGetReplies)))

	// Session endpoints
	s.mux.Handle("GET /api/session", authMw(http.HandlerFunc(s.handleGetCurrentSession)))
	s.mux.Handle("GET /api/sessions/{id}", authMw(http.HandlerFunc(s.handleGetSession)))

	// Project endpoints. The agent's project is derived from its session, so
	// /api/project (singular) returns the caller's project; /api/projects is
	// public discovery (mirrors /api/sessions).
	s.mux.Handle("GET /api/project", authMw(http.HandlerFunc(s.handleGetCurrentProject)))
	s.mux.HandleFunc("GET /api/projects", s.handleListProjects)

	// Admin endpoints
	s.mux.Handle("POST /api/admin/agents", adminMw(http.HandlerFunc(s.handleCreateAgent)))
	s.mux.Handle("POST /api/admin/projects", adminMw(http.HandlerFunc(s.handleCreateProject)))
	s.mux.Handle("GET /api/admin/projects", adminMw(http.HandlerFunc(s.handleAdminListProjects)))
	s.mux.Handle("POST /api/admin/projects/{slug}/import", adminMw(http.HandlerFunc(s.handleImportProject)))
	s.mux.Handle("POST /api/admin/sessions", adminMw(http.HandlerFunc(s.handleCreateSession)))
	s.mux.Handle("GET /api/admin/sessions", adminMw(http.HandlerFunc(s.handleListSessions)))
	s.mux.Handle("POST /api/admin/sessions/{id}/close", adminMw(http.HandlerFunc(s.handleCloseSession)))
	s.mux.Handle("DELETE /api/admin/sessions/{id}", adminMw(http.HandlerFunc(s.handleDeleteSession)))

	// Dashboard form actions (operator). Same admin middleware so a non-local
	// deploy still requires the admin bearer; in --no-auth mode they are open.
	s.mux.Handle("POST /admin/sessions/create", adminMw(http.HandlerFunc(s.handleDashboardCreateSession)))
	s.mux.Handle("POST /admin/sessions/{id}/close", adminMw(http.HandlerFunc(s.handleDashboardCloseSession)))
	s.mux.Handle("POST /admin/sessions/{id}/delete", adminMw(http.HandlerFunc(s.handleDashboardDeleteSession)))

	// Public onboarding (no auth): discover open sessions, register into one,
	// and read a self-describing guide so any agent can learn the hub cold.
	s.mux.HandleFunc("GET /api/sessions", s.handleListOpenSessions)
	s.mux.HandleFunc("POST /api/register", s.handleRegister)
	s.mux.HandleFunc("GET /api/guide", s.handleGuide)
	s.mux.HandleFunc("GET /llms.txt", s.handleGuide) // emerging convention for agent-readable docs
	s.mux.HandleFunc("GET /docs", s.handleDocs)       // human-readable HTML rendering of the guide

	// Health check (no auth) — also advertises the public onboarding routes.
	s.mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"status":   "ok",
			"docs":     "/docs",
			"guide":    "/api/guide",
			"sessions": "/api/sessions",
			"register": "/api/register",
		})
	})

	// Dashboard (no auth, public read-only)
	s.mux.HandleFunc("GET /", s.handleDashboard)
}

func (s *Server) ListenAndServe() error {
	log.Printf("listening on %s", s.config.ListenAddr)
	return http.ListenAndServe(s.config.ListenAddr, s.mux)
}

// JSON helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// requireOpenSession resolves the agent's session and rejects writes unless it
// is still open. Returns the session id and false if the request was rejected.
func (s *Server) requireOpenSession(w http.ResponseWriter, agent *db.Agent) (string, bool) {
	if agent.SessionID == "" {
		writeError(w, http.StatusForbidden, "agent is not bound to a session")
		return "", false
	}
	sess, err := s.db.GetSession(agent.SessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return "", false
	}
	if sess == nil {
		writeError(w, http.StatusForbidden, "agent session no longer exists")
		return "", false
	}
	if sess.Status != "open" {
		writeError(w, http.StatusConflict, "session is closed ("+sess.Status+"); no further writes accepted")
		return "", false
	}
	return sess.ID, true
}

// requireSession resolves the caller's session for read endpoints. Reads are
// allowed against closed sessions (archive), but an agent not bound to any
// session must not see the unscoped global view.
func (s *Server) requireSession(w http.ResponseWriter, agent *db.Agent) (string, bool) {
	if agent.SessionID == "" {
		writeError(w, http.StatusForbidden, "agent is not bound to a session")
		return "", false
	}
	return agent.SessionID, true
}

// sessionScope resolves the caller's session id and snapshot root for
// commit-visibility checks. Reads against closed sessions stay allowed.
func (s *Server) sessionScope(w http.ResponseWriter, agent *db.Agent) (sessionID, rootCommit string, ok bool) {
	sessionID, ok = s.requireSession(w, agent)
	if !ok {
		return "", "", false
	}
	if sess, _ := s.db.GetSession(sessionID); sess != nil {
		rootCommit = sess.RootCommit
	}
	return sessionID, rootCommit, true
}

// agentProject resolves the project an agent belongs to, via its session. An
// agent not bound to a session (or whose session/project vanished) yields nil.
func (s *Server) agentProject(agent *db.Agent) (*db.Project, error) {
	if agent == nil || agent.SessionID == "" {
		return nil, nil
	}
	sess, err := s.db.GetSession(agent.SessionID)
	if err != nil || sess == nil {
		return nil, err
	}
	return s.db.GetProjectByID(sess.ProjectID)
}

// requireProject resolves the caller's project (board scope) and rejects the
// request if the agent is not bound to a usable project.
func (s *Server) requireProject(w http.ResponseWriter, agent *db.Agent) (*db.Project, bool) {
	proj, err := s.agentProject(agent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return nil, false
	}
	if proj == nil {
		writeError(w, http.StatusForbidden, "agent is not bound to a project")
		return nil, false
	}
	return proj, true
}

// sessionRepoScope resolves session scope plus the per-project bare repo for
// git handlers that need both. The repo is the one owned by the agent's
// project.
func (s *Server) sessionRepoScope(w http.ResponseWriter, agent *db.Agent) (sessionID, rootCommit string, repo *gitrepo.Repo, ok bool) {
	sessionID, rootCommit, ok = s.sessionScope(w, agent)
	if !ok {
		return "", "", nil, false
	}
	proj, ok2 := s.requireProject(w, agent)
	if !ok2 {
		return "", "", nil, false
	}
	repo, err := s.getProjectRepo(proj.Slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to open project repo")
		return "", "", nil, false
	}
	return sessionID, rootCommit, repo, true
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	// Limit request body to 64KB for JSON endpoints
	limited := io.LimitReader(r.Body, 64*1024)
	return json.NewDecoder(limited).Decode(v)
}
