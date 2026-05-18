package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Model structs

type Session struct {
	ID        string     `json:"id"`
	Task      string     `json:"task"`
	Status    string     `json:"status"` // open | done | failed
	Result    string     `json:"result,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`
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

func (d *DB) Migrate() error {
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			task TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'open',
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
			name TEXT UNIQUE NOT NULL,
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
	} {
		if _, aerr := d.db.Exec(stmt); aerr != nil && !strings.Contains(aerr.Error(), "duplicate column name") {
			return aerr
		}
	}
	return nil
}

// --- Sessions ---

func (d *DB) CreateSession(id, task string) error {
	_, err := d.db.Exec("INSERT INTO sessions (id, task) VALUES (?, ?)", id, task)
	return err
}

func (d *DB) GetSession(id string) (*Session, error) {
	var s Session
	var closedAt sql.NullTime
	err := d.db.QueryRow(
		"SELECT id, task, status, result, created_at, closed_at FROM sessions WHERE id = ?", id,
	).Scan(&s.ID, &s.Task, &s.Status, &s.Result, &s.CreatedAt, &closedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if closedAt.Valid {
		s.ClosedAt = &closedAt.Time
	}
	return &s, err
}

func (d *DB) ListSessions() ([]Session, error) {
	rows, err := d.db.Query("SELECT id, task, status, result, created_at, closed_at FROM sessions ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		var s Session
		var closedAt sql.NullTime
		if err := rows.Scan(&s.ID, &s.Task, &s.Status, &s.Result, &s.CreatedAt, &closedAt); err != nil {
			return nil, err
		}
		if closedAt.Valid {
			s.ClosedAt = &closedAt.Time
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
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

// --- Agents ---

func (d *DB) CreateAgent(id, apiKey, sessionID string) error {
	_, err := d.db.Exec("INSERT INTO agents (id, api_key, session_id) VALUES (?, ?, ?)", id, apiKey, sessionID)
	return err
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
	_, err := d.db.Exec(
		"INSERT INTO commits (hash, parent_hash, agent_id, session_id, message) VALUES (?, ?, ?, ?, ?)",
		hash, parentHash, agentID, sessionID, message,
	)
	return err
}

func (d *DB) GetCommit(hash string) (*Commit, error) {
	var c Commit
	var parentHash, sessionID sql.NullString
	err := d.db.QueryRow(
		"SELECT hash, parent_hash, agent_id, session_id, message, created_at FROM commits WHERE hash = ?", hash,
	).Scan(&c.Hash, &parentHash, &c.AgentID, &sessionID, &c.Message, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if parentHash.Valid {
		c.ParentHash = parentHash.String
	}
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
	q += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
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

func (d *DB) GetLineage(hash string) ([]Commit, error) {
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
		lineage = append(lineage, *c)
		current = c.ParentHash
	}
	return lineage, nil
}

// GetLeaves returns frontier commits for a session (tips with no children).
// sessionID "" returns leaves across all sessions (operator/dashboard view).
func (d *DB) GetLeaves(sessionID string) ([]Commit, error) {
	q := `
		SELECT c.hash, c.parent_hash, c.agent_id, c.session_id, c.message, c.created_at
		FROM commits c
		LEFT JOIN commits child ON child.parent_hash = c.hash
		WHERE child.hash IS NULL`
	var args []any
	if sessionID != "" {
		q += " AND c.session_id = ?"
		args = append(args, sessionID)
	}
	q += " ORDER BY c.created_at DESC"
	rows, err := d.db.Query(q, args...)
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
		var parentHash, sessionID sql.NullString
		if err := rows.Scan(&c.Hash, &parentHash, &c.AgentID, &sessionID, &c.Message, &c.CreatedAt); err != nil {
			return nil, err
		}
		if parentHash.Valid {
			c.ParentHash = parentHash.String
		}
		c.SessionID = sessionID.String
		commits = append(commits, c)
	}
	return commits, rows.Err()
}

// --- Channels ---

func (d *DB) CreateChannel(name, description string) error {
	_, err := d.db.Exec("INSERT INTO channels (name, description) VALUES (?, ?)", name, description)
	return err
}

func (d *DB) ListChannels() ([]Channel, error) {
	rows, err := d.db.Query("SELECT id, name, description, created_at FROM channels ORDER BY name")
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

func (d *DB) GetChannelByName(name string) (*Channel, error) {
	var ch Channel
	err := d.db.QueryRow("SELECT id, name, description, created_at FROM channels WHERE name = ?", name).
		Scan(&ch.ID, &ch.Name, &ch.Description, &ch.CreatedAt)
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
	q += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
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

func (d *DB) ListSessionStats() ([]SessionStats, error) {
	sessions, err := d.ListSessions()
	if err != nil {
		return nil, err
	}
	var out []SessionStats
	for _, s := range sessions {
		ss := SessionStats{Session: s}
		d.db.QueryRow("SELECT COUNT(*) FROM agents WHERE session_id = ?", s.ID).Scan(&ss.AgentCount)
		d.db.QueryRow("SELECT COUNT(*) FROM commits WHERE session_id = ?", s.ID).Scan(&ss.CommitCount)
		d.db.QueryRow("SELECT COUNT(*) FROM posts WHERE session_id = ?", s.ID).Scan(&ss.PostCount)
		out = append(out, ss)
	}
	return out, nil
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

// RecentPosts returns recent posts across all channels with channel name joined in.
type PostWithChannel struct {
	Post
	ChannelName string
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
