package server

import (
	"io"
	"net/http"
	"os"
	"regexp"

	"agenthub/internal/auth"
	"agenthub/internal/db"
)

// Project slugs name a directory on disk ({dataDir}/projects/{slug}) and a URL
// segment, so keep them to the same conservative charset as channels/agents.
var projectSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,30}$`)

// handleCreateProject (admin) opens a new project and initializes its bare git
// repo on disk. Projects are the top-level grouping; sessions live inside one.
func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Slug        string `json:"slug"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !projectSlugRe.MatchString(req.Slug) {
		writeError(w, http.StatusBadRequest, "slug must be 1-31 lowercase alphanumeric/dash/underscore chars")
		return
	}
	if req.Name == "" {
		req.Name = req.Slug
	}

	existing, err := s.db.GetProjectBySlug(req.Slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if existing != nil {
		writeError(w, http.StatusConflict, "project already exists")
		return
	}

	if err := s.db.CreateProject(req.Slug, req.Name, req.Description); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create project")
		return
	}
	// Initialize the project's bare repo up front so the first push has a home.
	if _, err := s.getProjectRepo(req.Slug); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to init project repo: "+err.Error())
		return
	}

	proj, _ := s.db.GetProjectBySlug(req.Slug)
	writeJSON(w, http.StatusCreated, proj)
}

// handleAdminListProjects (admin) lists every project.
func (s *Server) handleAdminListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.db.ListProjects()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if projects == nil {
		projects = []db.Project{}
	}
	writeJSON(w, http.StatusOK, projects)
}

// handleListProjects is the public discovery endpoint (no auth): it lets an
// agent learn which projects exist before picking a session to join. Mirrors
// the public /api/sessions listing.
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.db.ListProjects()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if projects == nil {
		projects = []db.Project{}
	}
	writeJSON(w, http.StatusOK, projects)
}

// handleImportProject (admin) seeds — or updates — a project's git repo from an
// uploaded bundle (body = raw bundle bytes, same wire format as /api/git/push).
// This is the easy "move an existing repo into a project" path: bundle a local
// repo with `git bundle create <f> --all` and POST it here. The bundle's
// branches are mirrored into the project's bare repo so it tracks the source.
func (s *Server) handleImportProject(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	proj, err := s.db.GetProjectBySlug(slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if proj == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	repo, err := s.getProjectRepo(proj.Slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to open project repo")
		return
	}

	if s.config.MaxBundleSize > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, s.config.MaxBundleSize)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "bundle too large")
		return
	}

	tmpFile, err := os.CreateTemp("", "arhub-import-*.bundle")
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

	heads, err := repo.ImportBundle(tmpFile.Name())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid bundle: "+err.Error())
		return
	}

	// Imported commits aren't tied to a session; they become usable when a
	// session is opened with `--base <head>`, which indexes and freezes them.
	writeJSON(w, http.StatusCreated, map[string]any{
		"project": proj.Slug,
		"heads":   heads,
	})
}

// handleGetCurrentProject returns the project the calling agent belongs to,
// derived from its session.
func (s *Server) handleGetCurrentProject(w http.ResponseWriter, r *http.Request) {
	agent := auth.AgentFromContext(r.Context())
	proj, ok := s.requireProject(w, agent)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, proj)
}
