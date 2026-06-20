package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Model structs

type Project struct {
	ID          int       `json:"id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

type Session struct {
	ID         string     `json:"id"`
	ProjectID  int        `json:"project_id"`
	Task       string     `json:"task"`
	Status     string     `json:"status"` // open | done | failed
	RootCommit string     `json:"root_commit,omitempty"`
	Result     string     `json:"result,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ClosedAt   *time.Time `json:"closed_at,omitempty"`
}

type Agent struct {
	ID        string    `json:"id"`
	APIKey    string    `json:"api_key,omitempty"`
	SessionID string    `json:"session_id"`
	CreatedAt time.Time `json:"created_at"`
}

type Commit struct {
	Hash       string    `json:"hash"`
	ParentHash string    `json:"parent_hash"`
	AgentID    string    `json:"agent_id"`
	SessionID  string    `json:"session_id,omitempty"`
	Message    string    `json:"message"`
	CreatedAt  time.Time `json:"created_at"`
}

type Channel struct {
	ID          int       `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

type Post struct {
	ID        int       `json:"id"`
	ChannelID int       `json:"channel_id"`
	AgentID   string    `json:"agent_id"`
	SessionID string    `json:"session_id,omitempty"`
	ParentID  *int      `json:"parent_id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// DB wraps the SQLite connection.
type DB struct {
	db *sql.DB
}

func Open(path string) (*DB, error) {
	sqldb, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// SQLite pragmas for performance and correctness
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := sqldb.Exec(pragma); err != nil {
			sqldb.Close()
			return nil, fmt.Errorf("set pragma %q: %w", pragma, err)
		}
	}
	return &DB{db: sqldb}, nil
}

func (d *DB) Close() error {
	return d.db.Close()
}

// DefaultProjectSlug is the project every session lands in when none is
// specified. It is bootstrapped on migrate so the hub is usable out of the box
// and pre-project rows have a home.
const DefaultProjectSlug = "default"

func (d *DB) Migrate() error {
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS projects (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			slug TEXT UNIQUE NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			project_id INTEGER REFERENCES projects(id),
			task TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'open',
			root_commit TEXT NOT NULL DEFAULT '',
			result TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			closed_at TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS agents (
			id TEXT PRIMARY KEY,
			api_key TEXT UNIQUE NOT NULL,
			session_id TEXT REFERENCES sessions(id),
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS commits (
			hash TEXT PRIMARY KEY,
			parent_hash TEXT,
			agent_id TEXT REFERENCES agents(id),
			session_id TEXT,
			message TEXT,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS channels (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER REFERENCES projects(id),
			name TEXT NOT NULL,
			description TEXT DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS posts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			channel_id INTEGER NOT NULL REFERENCES channels(id),
			agent_id TEXT NOT NULL REFERENCES agents(id),
			session_id TEXT,
			parent_id INTEGER REFERENCES posts(id),
			content TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS rate_limits (
			agent_id TEXT NOT NULL,
			action TEXT NOT NULL,
			window_start TIMESTAMP NOT NULL,
			count INTEGER DEFAULT 1,
			PRIMARY KEY (agent_id, action, window_start)
		);

		CREATE INDEX IF NOT EXISTS idx_commits_parent ON commits(parent_hash);
		CREATE INDEX IF NOT EXISTS idx_commits_agent ON commits(agent_id);
		CREATE INDEX IF NOT EXISTS idx_commits_session ON commits(session_id);
		CREATE INDEX IF NOT EXISTS idx_posts_channel ON posts(channel_id);
		CREATE INDEX IF NOT EXISTS idx_posts_parent ON posts(parent_id);
		CREATE INDEX IF NOT EXISTS idx_posts_session ON posts(session_id);
	`)
	if err != nil {
		return err
	}
	// Backfill columns for pre-existing databases (CREATE TABLE IF NOT EXISTS
	// won't add them). Duplicate-column errors are expected and ignored.
	for _, stmt := range []string{
		"ALTER TABLE agents ADD COLUMN session_id TEXT REFERENCES sessions(id)",
		"ALTER TABLE commits ADD COLUMN session_id TEXT",
		"ALTER TABLE posts ADD COLUMN session_id TEXT",
		"ALTER TABLE sessions ADD COLUMN root_commit TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE sessions ADD COLUMN project_id INTEGER REFERENCES projects(id)",
		"ALTER TABLE channels ADD COLUMN project_id INTEGER REFERENCES projects(id)",
	} {
		if _, aerr := d.db.Exec(stmt); aerr != nil && !strings.Contains(aerr.Error(), "duplicate column name") {
			return aerr
		}
	}

	// Bootstrap the default project so every session/channel has a home.
	if _, err := d.db.Exec(
		"INSERT OR IGNORE INTO projects (slug, name, description) VALUES (?, ?, ?)",
		DefaultProjectSlug, "Default", "Default project",
	); err != nil {
		return err
	}
	var defaultID int
	if err := d.db.QueryRow("SELECT id FROM projects WHERE slug = ?", DefaultProjectSlug).Scan(&defaultID); err != nil {
		return err
	}
	// Adopt any pre-project rows into the default project.
	if _, err := d.db.Exec("UPDATE sessions SET project_id = ? WHERE project_id IS NULL", defaultID); err != nil {
		return err
	}
	if _, err := d.db.Exec("UPDATE channels SET project_id = ? WHERE project_id IS NULL", defaultID); err != nil {
		return err
	}

	// Channel names are unique per project (not globally). The unique index is
	// created after the backfill so existing rows carry a project_id first.
	if _, err := d.db.Exec(
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_channels_project_name ON channels(project_id, name)",
	); err != nil {
		return err
	}
	if _, err := d.db.Exec(
		"CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project_id)",
	); err != nil {
		return err
	}
	return nil
}

// --- Projects ---

func (d *DB) CreateProject(slug, name, description string) error {
	_, err := d.db.Exec(
		"INSERT INTO projects (slug, name, description) VALUES (?, ?, ?)",
		slug, name, description,
	)
	return err
}

func (d *DB) GetProjectBySlug(slug string) (*Project, error) {
	var p Project
	err := d.db.QueryRow(
		"SELECT id, slug, name, description, created_at FROM projects WHERE slug = ?", slug,
	).Scan(&p.ID, &p.Slug, &p.Name, &p.Description, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &p, err
}

func (d *DB) GetProjectByID(id int) (*Project, error) {
	var p Project
	err := d.db.QueryRow(
		"SELECT id, slug, name, description, created_at FROM projects WHERE id = ?", id,
	).Scan(&p.ID, &p.Slug, &p.Name, &p.Description, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &p, err
}

func (d *DB) ListProjects() ([]Project, error) {
	rows, err := d.db.Query("SELECT id, slug, name, description, created_at FROM projects ORDER BY slug")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var projects []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Slug, &p.Name, &p.Description, &p.CreatedAt); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// --- Sessions ---

func (d *DB) CreateSession(id, task, rootCommit string, projectID int) error {
	_, err := d.db.Exec(
		"INSERT INTO sessions (id, project_id, task, root_commit) VALUES (?, ?, ?, ?)",
		id, projectID, task, rootCommit,
	)
	return err
}

func (d *DB) GetSession(id string) (*Session, error) {
	var s Session
	var closedAt sql.NullTime
	var projectID sql.NullInt64
	err := d.db.QueryRow(
		"SELECT id, project_id, task, status, root_commit, result, created_at, closed_at FROM sessions WHERE id = ?", id,
	).Scan(&s.ID, &projectID, &s.Task, &s.Status, &s.RootCommit, &s.Result, &s.CreatedAt, &closedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	s.ProjectID = int(projectID.Int64)
	if closedAt.Valid {
		s.ClosedAt = &closedAt.Time
	}
	return &s, err
}

func (d *DB) ListSessions() ([]Session, error) {
	rows, err := d.db.Query("SELECT id, project_id, task, status, root_commit, result, created_at, closed_at FROM sessions ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		var s Session
		var closedAt sql.NullTime
		var projectID sql.NullInt64
		if err := rows.Scan(&s.ID, &projectID, &s.Task, &s.Status, &s.RootCommit, &s.Result, &s.CreatedAt, &closedAt); err != nil {
			return nil, err
		}
		s.ProjectID = int(projectID.Int64)
		if closedAt.Valid {
			s.ClosedAt = &closedAt.Time
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

func (d *DB) CountAgentsInSession(sessionID string) (int, error) {
	var n int
	err := d.db.QueryRow("SELECT COUNT(*) FROM agents WHERE session_id = ?", sessionID).Scan(&n)
	return n, err
}

// CloseSession sets a terminal status and result. status must be done or failed.
func (d *DB) CloseSession(id, status, result string) error {
	res, err := d.db.Exec(
		"UPDATE sessions SET status = ?, result = ?, closed_at = CURRENT_TIMESTAMP WHERE id = ? AND status = 'open'",
		status, result, id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("session not found or already closed")
	}
	return nil
}

// DeleteSession removes a session and everything tied to it (posts, commits,
// agents, rate-limit counters). Wrapped in a transaction so the cascade is
// all-or-nothing.
func (d *DB) DeleteSession(id string) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM posts WHERE session_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM commits WHERE session_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.Exec(
		"DELETE FROM rate_limits WHERE agent_id IN (SELECT id FROM agents WHERE session_id = ?)", id,
	); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM agents WHERE session_id = ?", id); err != nil {
		return err
	}
	res, err := tx.Exec("DELETE FROM sessions WHERE id = ?", id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("session not found")
	}
	return tx.Commit()
}

// --- Agents ---

func (d *DB) CreateAgent(id, apiKey, sessionID string) error {
	// Store NULL (not "") when unbound so the sessions foreign key holds.
	var sess any
	if sessionID != "" {
		sess = sessionID
	}
	_, err := d.db.Exec("INSERT INTO agents (id, api_key, session_id) VALUES (?, ?, ?)", id, apiKey, sess)
	return err
}

// CreateAgentCapped atomically creates an agent only if the session is below
// maxAgents (<=0 means unlimited). Returns false if the cap was reached.
func (d *DB) CreateAgentCapped(id, apiKey, sessionID string, maxAgents int) (bool, error) {
	if maxAgents <= 0 {
		return true, d.CreateAgent(id, apiKey, sessionID)
	}
	res, err := d.db.Exec(`
		INSERT INTO agents (id, api_key, session_id)
		SELECT ?, ?, ?
		WHERE (SELECT COUNT(*) FROM agents WHERE session_id = ?) < ?`,
		id, apiKey, sessionID, sessionID, maxAgents,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CommitVisibleInSession reports whether a commit hash is reachable from a
// session: either it was made in that session, or it is the session's frozen
// snapshot root (whose row may belong to another session).
func (d *DB) CommitVisibleInSession(hash, sessionID, rootCommit string) (bool, error) {
	var one int
	err := d.db.QueryRow(
		"SELECT 1 FROM commits WHERE hash = ? AND (session_id = ? OR hash = ?)",
		hash, sessionID, rootCommit,
	).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// GetLineageScoped walks ancestry but stays within the session: it stops once
// it reaches the snapshot root (inclusive) or leaves the session's scope, so
// pre-snapshot history of other sessions is never exposed.
func (d *DB) GetLineageScoped(hash, sessionID, rootCommit string) ([]Commit, error) {
	var lineage []Commit
	current := hash
	for current != "" {
		c, err := d.GetCommit(current)
		if err != nil {
			return lineage, err
		}
		if c == nil {
			break
		}
		if c.SessionID != sessionID && c.Hash != rootCommit {
			break
		}
		lineage = append(lineage, *c)
		if c.Hash == rootCommit {
			break
		}
		current = c.ParentHash
	}
	return lineage, nil
}

func (d *DB) GetAgentByAPIKey(apiKey string) (*Agent, error) {
	var a Agent
	var sessionID sql.NullString
	err := d.db.QueryRow("SELECT id, api_key, session_id, created_at FROM agents WHERE api_key = ?", apiKey).
		Scan(&a.ID, &a.APIKey, &sessionID, &a.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	a.SessionID = sessionID.String
	return &a, err
}

func (d *DB) GetAgentByID(id string) (*Agent, error) {
	var a Agent
	var sessionID sql.NullString
	err := d.db.QueryRow("SELECT id, api_key, session_id, created_at FROM agents WHERE id = ?", id).
		Scan(&a.ID, &a.APIKey, &sessionID, &a.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	a.SessionID = sessionID.String
	return &a, err
}

// --- Commits ---

func (d *DB) InsertCommit(hash, parentHash, agentID, sessionID, message string) error {
	// Seed/ancestor commits have no author; store NULL so the agents
	// foreign key is not violated (empty string would not match any agent).
	var agent any
	if agentID != "" {
		agent = agentID
	}
	_, err := d.db.Exec(
		"INSERT INTO commits (hash, parent_hash, agent_id, session_id, message) VALUES (?, ?, ?, ?, ?)",
		hash, parentHash, agent, sessionID, message,
	)
	return err
}

func (d *DB) GetCommit(hash string) (*Commit, error) {
	var c Commit
	var parentHash, agentID, sessionID sql.NullString
	err := d.db.QueryRow(
		"SELECT hash, parent_hash, agent_id, session_id, message, created_at FROM commits WHERE hash = ?", hash,
	).Scan(&c.Hash, &parentHash, &agentID, &sessionID, &c.Message, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if parentHash.Valid {
		c.ParentHash = parentHash.String
	}
	c.AgentID = agentID.String
	c.SessionID = sessionID.String
	return &c, err
}

// ListCommits filters by agentID and/or sessionID; empty string means no filter.
func (d *DB) ListCommits(agentID, sessionID string, limit, offset int) ([]Commit, error) {
	if limit <= 0 {
		limit = 50
	}
	q := "SELECT hash, parent_hash, agent_id, session_id, message, created_at FROM commits WHERE 1=1"
	var args []any
	if agentID != "" {
		q += " AND agent_id = ?"
		args = append(args, agentID)
	}
	if sessionID != "" {
		q += " AND session_id = ?"
		args = append(args, sessionID)
	}
	// rowid DESC tiebreaker: bursty workers can stamp many rows in the same
	// second, and "give me the latest N" should mean most-recently-inserted
	// N, not whatever SQLite happens to return for the timestamp tie. rowid
	// works for both posts (rowid == id) and commits (which has no explicit
	// id column).
	q += " ORDER BY created_at DESC, rowid DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCommits(rows)
}

// GetChildren returns child commits; sessionID "" means no session filter.
func (d *DB) GetChildren(hash, sessionID string) ([]Commit, error) {
	q := "SELECT hash, parent_hash, agent_id, session_id, message, created_at FROM commits WHERE parent_hash = ?"
	args := []any{hash}
	if sessionID != "" {
		q += " AND session_id = ?"
		args = append(args, sessionID)
	}
	q += " ORDER BY created_at DESC"
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCommits(rows)
}

// GetLeaves returns frontier commits for a session (tips with no children).
// The scope is the session's own commits plus its frozen root snapshot, so a
// brand-new session's frontier is the snapshot until the swarm builds on it.
// sessionID "" returns leaves across all sessions (operator/dashboard view).
func (d *DB) GetLeaves(sessionID, rootCommit string) ([]Commit, error) {
	if sessionID == "" {
		rows, err := d.db.Query(`
			SELECT c.hash, c.parent_hash, c.agent_id, c.session_id, c.message, c.created_at
			FROM commits c
			LEFT JOIN commits child ON child.parent_hash = c.hash
			WHERE child.hash IS NULL
			ORDER BY c.created_at DESC`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return scanCommits(rows)
	}
	rows, err := d.db.Query(`
		WITH scope AS (
			SELECT hash, parent_hash, agent_id, session_id, message, created_at
			FROM commits WHERE session_id = ?
			UNION
			SELECT hash, parent_hash, agent_id, session_id, message, created_at
			FROM commits WHERE hash = ?
		)
		SELECT s.hash, s.parent_hash, s.agent_id, s.session_id, s.message, s.created_at
		FROM scope s
		LEFT JOIN scope child ON child.parent_hash = s.hash
		WHERE child.hash IS NULL
		ORDER BY s.created_at DESC`, sessionID, rootCommit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCommits(rows)
}

func scanCommits(rows *sql.Rows) ([]Commit, error) {
	var commits []Commit
	for rows.Next() {
		var c Commit
		var parentHash, agentID, sessionID sql.NullString
		if err := rows.Scan(&c.Hash, &parentHash, &agentID, &sessionID, &c.Message, &c.CreatedAt); err != nil {
			return nil, err
		}
		if parentHash.Valid {
			c.ParentHash = parentHash.String
		}
		c.AgentID = agentID.String
		c.SessionID = sessionID.String
		commits = append(commits, c)
	}
	return commits, rows.Err()
}

// --- Channels ---

func (d *DB) CreateChannel(projectID int, name, description string) error {
	_, err := d.db.Exec(
		"INSERT INTO channels (project_id, name, description) VALUES (?, ?, ?)",
		projectID, name, description,
	)
	return err
}

func (d *DB) ListChannels(projectID int) ([]Channel, error) {
	rows, err := d.db.Query(
		"SELECT id, name, description, created_at FROM channels WHERE project_id = ? ORDER BY name",
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var channels []Channel
	for rows.Next() {
		var ch Channel
		if err := rows.Scan(&ch.ID, &ch.Name, &ch.Description, &ch.CreatedAt); err != nil {
			return nil, err
		}
		channels = append(channels, ch)
	}
	return channels, rows.Err()
}

func (d *DB) GetChannelByName(projectID int, name string) (*Channel, error) {
	var ch Channel
	err := d.db.QueryRow(
		"SELECT id, name, description, created_at FROM channels WHERE project_id = ? AND name = ?",
		projectID, name,
	).Scan(&ch.ID, &ch.Name, &ch.Description, &ch.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &ch, err
}

// --- Posts ---

func (d *DB) CreatePost(channelID int, agentID, sessionID string, parentID *int, content string) (*Post, error) {
	res, err := d.db.Exec(
		"INSERT INTO posts (channel_id, agent_id, session_id, parent_id, content) VALUES (?, ?, ?, ?, ?)",
		channelID, agentID, sessionID, parentID, content,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return d.GetPost(int(id))
}

// ListPosts returns posts in a channel; sessionID "" means no session filter.
func (d *DB) ListPosts(channelID int, sessionID string, limit, offset int) ([]Post, error) {
	if limit <= 0 {
		limit = 50
	}
	q := "SELECT id, channel_id, agent_id, session_id, parent_id, content, created_at FROM posts WHERE channel_id = ?"
	args := []any{channelID}
	if sessionID != "" {
		q += " AND session_id = ?"
		args = append(args, sessionID)
	}
	// rowid DESC tiebreaker: bursty workers can stamp many rows in the same
	// second, and "give me the latest N" should mean most-recently-inserted
	// N, not whatever SQLite happens to return for the timestamp tie. rowid
	// works for both posts (rowid == id) and commits (which has no explicit
	// id column).
	q += " ORDER BY created_at DESC, rowid DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPosts(rows)
}

func (d *DB) GetPost(id int) (*Post, error) {
	var p Post
	var parentID sql.NullInt64
	var sessionID sql.NullString
	err := d.db.QueryRow(
		"SELECT id, channel_id, agent_id, session_id, parent_id, content, created_at FROM posts WHERE id = ?", id,
	).Scan(&p.ID, &p.ChannelID, &p.AgentID, &sessionID, &parentID, &p.Content, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	p.SessionID = sessionID.String
	if parentID.Valid {
		v := int(parentID.Int64)
		p.ParentID = &v
	}
	return &p, err
}

func (d *DB) GetReplies(postID int) ([]Post, error) {
	rows, err := d.db.Query(
		"SELECT id, channel_id, agent_id, session_id, parent_id, content, created_at FROM posts WHERE parent_id = ? ORDER BY created_at ASC",
		postID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPosts(rows)
}

func scanPosts(rows *sql.Rows) ([]Post, error) {
	var posts []Post
	for rows.Next() {
		var p Post
		var parentID sql.NullInt64
		var sessionID sql.NullString
		if err := rows.Scan(&p.ID, &p.ChannelID, &p.AgentID, &sessionID, &parentID, &p.Content, &p.CreatedAt); err != nil {
			return nil, err
		}
		p.SessionID = sessionID.String
		if parentID.Valid {
			v := int(parentID.Int64)
			p.ParentID = &v
		}
		posts = append(posts, p)
	}
	return posts, rows.Err()
}

// --- Dashboard queries ---

type Stats struct {
	SessionCount int
	OpenSessions int
	AgentCount   int
	CommitCount  int
	PostCount    int
}

func (d *DB) GetStats() (*Stats, error) {
	var s Stats
	d.db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&s.SessionCount)
	d.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE status = 'open'").Scan(&s.OpenSessions)
	d.db.QueryRow("SELECT COUNT(*) FROM agents").Scan(&s.AgentCount)
	d.db.QueryRow("SELECT COUNT(*) FROM commits").Scan(&s.CommitCount)
	d.db.QueryRow("SELECT COUNT(*) FROM posts").Scan(&s.PostCount)
	return &s, nil
}

// SessionStats is per-session activity for the dashboard.
type SessionStats struct {
	Session
	AgentCount  int
	CommitCount int
	PostCount   int
}

// ListSessionStats returns per-session activity counts. projectID 0 means all
// projects (operator/global view); a positive id scopes to one project.
func (d *DB) ListSessionStats(projectID int) ([]SessionStats, error) {
	q := `
		SELECT s.id, s.project_id, s.task, s.status, s.root_commit, s.result, s.created_at, s.closed_at,
			(SELECT COUNT(*) FROM agents  a WHERE a.session_id = s.id),
			(SELECT COUNT(*) FROM commits c WHERE c.session_id = s.id),
			(SELECT COUNT(*) FROM posts   p WHERE p.session_id = s.id)
		FROM sessions s`
	var args []any
	if projectID > 0 {
		q += " WHERE s.project_id = ?"
		args = append(args, projectID)
	}
	q += " ORDER BY s.created_at DESC"
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionStats
	for rows.Next() {
		var ss SessionStats
		var closedAt sql.NullTime
		var projID sql.NullInt64
		if err := rows.Scan(&ss.ID, &projID, &ss.Task, &ss.Status, &ss.RootCommit, &ss.Result,
			&ss.CreatedAt, &closedAt, &ss.AgentCount, &ss.CommitCount, &ss.PostCount); err != nil {
			return nil, err
		}
		ss.ProjectID = int(projID.Int64)
		if closedAt.Valid {
			ss.ClosedAt = &closedAt.Time
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

func (d *DB) ListAgents() ([]Agent, error) {
	rows, err := d.db.Query("SELECT id, '', COALESCE(session_id, ''), created_at FROM agents ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.APIKey, &a.SessionID, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.APIKey = "" // never expose
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// ListAgentsInSession returns all agents bound to a session (api keys masked).
func (d *DB) ListAgentsInSession(sessionID string) ([]Agent, error) {
	rows, err := d.db.Query(
		"SELECT id, '', COALESCE(session_id, ''), created_at FROM agents WHERE session_id = ? ORDER BY created_at",
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.APIKey, &a.SessionID, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.APIKey = ""
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// RecentPosts returns recent posts across all channels with channel name joined in.
type PostWithChannel struct {
	Post
	ChannelName string
}

// RecentPostsForSession returns recent posts in a single session.
func (d *DB) RecentPostsForSession(sessionID string, limit int) ([]PostWithChannel, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.db.Query(`
		SELECT p.id, p.channel_id, p.agent_id, COALESCE(p.session_id, ''), p.parent_id, p.content, p.created_at, c.name
		FROM posts p JOIN channels c ON p.channel_id = c.id
		WHERE p.session_id = ?
		ORDER BY p.created_at DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var posts []PostWithChannel
	for rows.Next() {
		var p PostWithChannel
		var parentID sql.NullInt64
		if err := rows.Scan(&p.ID, &p.ChannelID, &p.AgentID, &p.SessionID, &parentID, &p.Content, &p.CreatedAt, &p.ChannelName); err != nil {
			return nil, err
		}
		if parentID.Valid {
			v := int(parentID.Int64)
			p.ParentID = &v
		}
		posts = append(posts, p)
	}
	return posts, rows.Err()
}

func (d *DB) RecentPosts(limit int) ([]PostWithChannel, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.db.Query(`
		SELECT p.id, p.channel_id, p.agent_id, COALESCE(p.session_id, ''), p.parent_id, p.content, p.created_at, c.name
		FROM posts p JOIN channels c ON p.channel_id = c.id
		ORDER BY p.created_at DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var posts []PostWithChannel
	for rows.Next() {
		var p PostWithChannel
		var parentID sql.NullInt64
		if err := rows.Scan(&p.ID, &p.ChannelID, &p.AgentID, &p.SessionID, &parentID, &p.Content, &p.CreatedAt, &p.ChannelName); err != nil {
			return nil, err
		}
		if parentID.Valid {
			v := int(parentID.Int64)
			p.ParentID = &v
		}
		posts = append(posts, p)
	}
	return posts, rows.Err()
}

// --- Rate Limiting ---

// CheckRateLimit returns true if the agent is within the allowed rate.
func (d *DB) CheckRateLimit(agentID, action string, maxPerHour int) (bool, error) {
	var count int
	err := d.db.QueryRow(
		"SELECT COALESCE(SUM(count), 0) FROM rate_limits WHERE agent_id = ? AND action = ? AND window_start > datetime('now', '-1 hour')",
		agentID, action,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count < maxPerHour, nil
}

func (d *DB) IncrementRateLimit(agentID, action string) error {
	_, err := d.db.Exec(`
		INSERT INTO rate_limits (agent_id, action, window_start, count)
		VALUES (?, ?, strftime('%Y-%m-%d %H:%M:00', 'now'), 1)
		ON CONFLICT(agent_id, action, window_start) DO UPDATE SET count = count + 1
	`, agentID, action)
	return err
}

func (d *DB) CleanupRateLimits() error {
	_, err := d.db.Exec("DELETE FROM rate_limits WHERE window_start < datetime('now', '-2 hours')")
	return err
}
