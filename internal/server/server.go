package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	"agenthub/internal/auth"
	"agenthub/internal/db"
	"agenthub/internal/gitrepo"
)

type Config struct {
	MaxBundleSize       int64  // max bundle upload size in bytes
	MaxPushesPerHour    int    // per agent
	MaxPostsPerHour     int    // per agent
	MaxAgentsPerSession int    // 0 = unlimited
	ListenAddr          string // e.g. ":8080"
}

type Server struct {
	db       *db.DB
	repo     *gitrepo.Repo
	adminKey string
	mux      *http.ServeMux
	config   Config
}

func New(database *db.DB, repo *gitrepo.Repo, adminKey string, cfg Config) *Server {
	s := &Server{
		db:       database,
		repo:     repo,
		adminKey: adminKey,
		mux:      http.NewServeMux(),
		config:   cfg,
	}
	s.setupRoutes()
	return s
}

func (s *Server) setupRoutes() {
	authMw := auth.Middleware(s.db)
	adminMw := auth.AdminMiddleware(s.adminKey)

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

	// Admin endpoints
	s.mux.Handle("POST /api/admin/agents", adminMw(http.HandlerFunc(s.handleCreateAgent)))
	s.mux.Handle("POST /api/admin/sessions", adminMw(http.HandlerFunc(s.handleCreateSession)))
	s.mux.Handle("GET /api/admin/sessions", adminMw(http.HandlerFunc(s.handleListSessions)))
	s.mux.Handle("POST /api/admin/sessions/{id}/close", adminMw(http.HandlerFunc(s.handleCloseSession)))

	// Public registration (no auth, rate-limited by IP)
	s.mux.HandleFunc("POST /api/register", s.handleRegister)

	// Health check (no auth)
	s.mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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

// sessionAtAgentCap reports whether the session already holds the configured
// maximum number of agents (0 = unlimited).
func (s *Server) sessionAtAgentCap(sessionID string) (bool, error) {
	if s.config.MaxAgentsPerSession <= 0 {
		return false, nil
	}
	n, err := s.db.CountAgentsInSession(sessionID)
	if err != nil {
		return false, err
	}
	return n >= s.config.MaxAgentsPerSession, nil
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	// Limit request body to 64KB for JSON endpoints
	limited := io.LimitReader(r.Body, 64*1024)
	return json.NewDecoder(limited).Decode(v)
}
