package server

import (
	"html/template"
	"net/http"
	"strconv"
	"time"

	"agenthub/internal/db"
	"agenthub/internal/gitrepo"
)

type dashboardData struct {
	Stats       *db.Stats
	Projects    []db.Project
	CurrProject *db.Project          // nil = all projects (no ?p=)
	Sessions    []db.SessionStats
	Selected    *db.SessionStats     // nil when no ?s= is selected
	Agents      []db.Agent           // agents in the selected session
	Commits     []db.Commit          // commits in the selected session
	Posts       []db.PostWithChannel // posts in the selected session
	Mutable     bool                 // true when the dashboard may show write actions
	AutoReload  bool                 // refresh meta tag enabled
	Now         time.Time
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	stats, _ := s.db.GetStats()
	projects, _ := s.db.ListProjects()

	data := dashboardData{
		Stats:      stats,
		Projects:   projects,
		Mutable:    s.config.NoAuth,
		AutoReload: true,
		Now:        time.Now().UTC(),
	}

	// ?p=<slug> scopes the session list to one project; absent = all projects.
	projectID := 0
	if slug := r.URL.Query().Get("p"); slug != "" {
		if proj, _ := s.db.GetProjectBySlug(slug); proj != nil {
			data.CurrProject = proj
			projectID = proj.ID
		}
	}
	sessions, _ := s.db.ListSessionStats(projectID)
	data.Sessions = sessions

	if id := r.URL.Query().Get("s"); id != "" {
		for i := range sessions {
			if sessions[i].ID == id {
				data.Selected = &sessions[i]
				break
			}
		}
		if data.Selected != nil {
			data.Agents, _ = s.db.ListAgentsInSession(id)
			data.Commits, _ = s.db.ListCommits("", id, 200, 0)
			data.Posts, _ = s.db.RecentPostsForSession(id, 200)
			data.AutoReload = false // don't disrupt the operator mid-action
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	dashboardTmpl.Execute(w, data)
}

// --- Dashboard form actions (admin-only; open in --no-auth mode) ---

func (s *Server) handleDashboardCreateSession(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	sess, status, errMsg := s.createSession(r.FormValue("id"), r.FormValue("task"), r.FormValue("base"), r.FormValue("project"))
	if errMsg != "" {
		http.Error(w, errMsg, status)
		return
	}
	http.Redirect(w, r, "/?s="+sess.ID, http.StatusSeeOther)
}

func (s *Server) handleDashboardCloseSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	status := r.FormValue("status")
	if status == "" {
		status = "done"
	}
	if status != "done" && status != "failed" {
		http.Error(w, "status must be done or failed", http.StatusBadRequest)
		return
	}
	result := r.FormValue("result")
	if result == "" {
		if c, sum := r.FormValue("commit"), r.FormValue("summary"); c != "" || sum != "" {
			result = "commit=" + c + "\n" + sum
		}
	}
	if err := s.db.CloseSession(id, status, result); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/?s="+id, http.StatusSeeOther)
}

func (s *Server) handleDashboardDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Resolve the owning project's repo before the row is deleted.
	var repo *gitrepo.Repo
	if sess, _ := s.db.GetSession(id); sess != nil {
		if proj, _ := s.db.GetProjectByID(sess.ProjectID); proj != nil {
			repo, _ = s.getProjectRepo(proj.Slug)
		}
	}
	if err := s.db.DeleteSession(id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if repo != nil {
		repo.DeleteRef("refs/sessions/" + id)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- Template helpers ---

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1m ago"
		}
		return itoa(m) + "m ago"
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1h ago"
		}
		return itoa(h) + "h ago"
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1d ago"
		}
		return itoa(days) + "d ago"
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

var funcMap = template.FuncMap{
	"short":    shortHash,
	"truncate": truncate,
	"timeago":  timeAgo,
}

var dashboardTmpl = template.Must(template.New("dashboard").Funcs(funcMap).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>agenthub</title>
{{if .AutoReload}}<meta http-equiv="refresh" content="30">{{end}}
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  html, body { height: 100%; }
  body {
    font-family: 'SF Mono','Menlo','Consolas',monospace;
    background: #0a0a0a; color: #e0e0e0; font-size: 13px; line-height: 1.55;
    display: grid; grid-template-columns: 280px 1fr; height: 100vh; overflow: hidden;
  }
  a { color: inherit; text-decoration: none; }
  button, input[type=submit] { font: inherit; color: inherit; }

  /* Sidebar */
  .sidebar {
    background: #0d0d0d; border-right: 1px solid #1a1a1a;
    display: flex; flex-direction: column; overflow: hidden;
  }
  .sidebar header {
    padding: 16px 16px 12px; border-bottom: 1px solid #1a1a1a;
  }
  .sidebar h1 { font-size: 15px; color: #fff; letter-spacing: 1px; }
  .sidebar .sub { font-size: 11px; color: #555; margin-top: 2px; }
  .project-switcher {
    padding: 12px 16px; border-bottom: 1px solid #1a1a1a;
  }
  .switcher-label { font-size: 10px; color: #666; text-transform: uppercase;
    letter-spacing: 1px; margin-bottom: 6px; }
  .project-list { display: flex; flex-wrap: wrap; gap: 4px; }
  .project-item {
    background: #141414; border: 1px solid #222; color: #aaa;
    padding: 3px 9px; border-radius: 12px; font-size: 11px; cursor: pointer;
  }
  .project-item:hover { background: #1a1a1a; border-color: #333; }
  .project-item.active { background: #1a1a2e; color: #7aa2f7; border-color: #2a3a5a; }
  .new-session {
    padding: 12px 16px; border-bottom: 1px solid #1a1a1a;
    display: flex; flex-direction: column; gap: 6px;
  }
  .new-session input[type=text], .new-session select {
    background: #141414; border: 1px solid #222; color: #e0e0e0;
    padding: 7px 10px; border-radius: 4px; font: inherit; outline: none;
  }
  .new-session input[type=text]:focus, .new-session select:focus { border-color: #3a3a3a; }
  .new-session button {
    background: #1a2e1a; color: #7af7a2; border: 1px solid #25422a;
    padding: 7px; border-radius: 4px; cursor: pointer;
  }
  .new-session button:hover { background: #213c21; }
  .session-list { flex: 1; overflow-y: auto; padding: 8px 0; }
  .session-item {
    display: block; padding: 10px 16px; border-left: 2px solid transparent;
    color: #ccc; cursor: pointer; border-bottom: 1px solid #111;
  }
  .session-item:hover { background: #141414; }
  .session-item.active { background: #141414; border-left-color: #7aa2f7; }
  .session-row { display: flex; align-items: center; gap: 6px; }
  .dot { width: 8px; height: 8px; border-radius: 50%; flex-shrink: 0; }
  .dot.open   { background: #7af7a2; box-shadow: 0 0 4px #7af7a2; }
  .dot.done   { background: #81a2be; }
  .dot.failed { background: #f07a7a; }
  .session-title { color: #ddd; font-size: 12px; flex: 1; min-width: 0;
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .session-id { color: #555; font-size: 11px; margin-top: 2px; }
  .empty { color: #444; font-style: italic; padding: 16px; }

  /* Main pane */
  .main { overflow-y: auto; padding: 24px 32px 64px; }
  .panel { background: #0f0f0f; border: 1px solid #1a1a1a; border-radius: 8px;
    padding: 16px 20px; margin-bottom: 16px; }
  h2 { font-size: 11px; color: #888; text-transform: uppercase; letter-spacing: 1.5px;
    margin-bottom: 10px; }
  .pane-header { display: flex; align-items: center; gap: 12px; margin-bottom: 16px; }
  .pane-header .id { font-size: 18px; color: #fff; }
  .pane-header .badge { padding: 3px 8px; border-radius: 3px; font-size: 11px;
    text-transform: uppercase; letter-spacing: 1px; }
  .badge.open   { background: #1a2e1a; color: #7af7a2; }
  .badge.done   { background: #1a242e; color: #81a2be; }
  .badge.failed { background: #2e1a1a; color: #f07a7a; }
  .task { color: #ccc; font-size: 14px; margin-bottom: 16px; }
  .meta { color: #777; font-size: 12px; }
  .meta b { color: #aaa; font-weight: normal; }
  .hash { color: #f0c674; }

  .stats { display: flex; gap: 12px; margin-bottom: 16px; flex-wrap: wrap; }
  .stat { background: #141414; border: 1px solid #1a1a1a; border-radius: 6px;
    padding: 10px 14px; min-width: 90px; }
  .stat-value { font-size: 20px; color: #fff; font-weight: 600; }
  .stat-label { font-size: 10px; color: #666; text-transform: uppercase;
    letter-spacing: 1px; margin-top: 2px; }

  .actions { display: flex; gap: 8px; margin-bottom: 16px; }
  .actions form { display: inline-flex; gap: 6px; align-items: center; }
  .actions input[type=text] {
    background: #141414; border: 1px solid #222; color: #e0e0e0;
    padding: 6px 10px; border-radius: 4px; font: inherit; outline: none; min-width: 200px;
  }
  .btn { padding: 6px 12px; border-radius: 4px; cursor: pointer;
    background: #141414; border: 1px solid #2a2a2a; color: #ccc; }
  .btn:hover { background: #1a1a1a; }
  .btn.warn  { color: #f07a7a; border-color: #3a1f1f; }
  .btn.warn:hover  { background: #2a1414; }
  .btn.ok    { color: #7af7a2; border-color: #1f3a25; }
  .btn.ok:hover    { background: #142a18; }

  table { width: 100%; border-collapse: collapse; }
  th { text-align: left; color: #666; font-size: 10px; text-transform: uppercase;
    letter-spacing: 1px; padding: 6px 8px; border-bottom: 1px solid #1a1a1a; }
  td { padding: 6px 8px; border-bottom: 1px solid #111; vertical-align: top; font-size: 12px; }
  .agent { color: #81a2be; }
  .msg { color: #b5bd68; }
  .time { color: #555; font-size: 11px; }

  .post { background: #141414; border: 1px solid #1a1a1a; border-radius: 6px;
    padding: 10px 14px; margin-bottom: 8px; }
  .post-header { display: flex; gap: 8px; align-items: center; margin-bottom: 4px; font-size: 11px; }
  .channel-tag { background: #1a1a2e; color: #7aa2f7; padding: 2px 6px; border-radius: 3px; }
  .post-content { color: #ccc; white-space: pre-wrap; word-break: break-word; font-size: 12px; }
  .reply-indicator { color: #555; }

  .empty-state { text-align: center; padding: 80px 20px; color: #555; }
  .empty-state h3 { color: #888; font-size: 16px; margin-bottom: 8px; }
</style>
</head>
<body>

<aside class="sidebar">
  <header>
    <h1>agenthub</h1>
    <div class="sub">{{if .Mutable}}local mode{{else}}read-only{{end}} · {{.Stats.OpenSessions}}/{{.Stats.SessionCount}} sessions open · <a href="/docs">docs</a></div>
  </header>

  <div class="project-switcher">
    <div class="switcher-label">Project</div>
    <div class="project-list">
      <a class="project-item{{if not .CurrProject}} active{{end}}" href="/">all</a>
      {{range .Projects}}
      <a class="project-item{{if and $.CurrProject (eq .Slug $.CurrProject.Slug)}} active{{end}}" href="/?p={{.Slug}}">{{.Slug}}</a>
      {{end}}
    </div>
  </div>

  {{if .Mutable}}
  <form class="new-session" method="post" action="/admin/sessions/create">
    <input type="text" name="task" placeholder="task for the swarm…" required>
    <input type="text" name="base" placeholder="base commit (optional)">
    <select name="project">
      {{range .Projects}}<option value="{{.Slug}}"{{if $.CurrProject}}{{if eq .Slug $.CurrProject.Slug}} selected{{end}}{{else if eq .Slug "default"}} selected{{end}}>{{.Slug}}</option>{{end}}
    </select>
    <button type="submit">+ new session</button>
  </form>
  {{end}}

  <div class="session-list">
    {{if .Sessions}}
      {{range .Sessions}}
      <a class="session-item{{if and $.Selected (eq .ID $.Selected.ID)}} active{{end}}" href="/?s={{.ID}}">
        <div class="session-row">
          <span class="dot {{.Status}}"></span>
          <span class="session-title">{{if .Task}}{{truncate .Task 40}}{{else}}(no task){{end}}</span>
        </div>
        <div class="session-id">{{.ID}} · {{.AgentCount}} agents · {{.CommitCount}} commits</div>
      </a>
      {{end}}
    {{else}}
      <div class="empty">no sessions yet</div>
    {{end}}
  </div>
</aside>

<main class="main">
{{if .Selected}}
  {{with .Selected}}
  <div class="pane-header">
    <span class="id">{{.ID}}</span>
    <span class="badge {{.Status}}">{{.Status}}</span>
    <span class="time">started {{timeago .CreatedAt}}{{if .ClosedAt}} · closed {{timeago .ClosedAt}}{{end}}</span>
  </div>
  <div class="task">{{if .Task}}{{.Task}}{{else}}<i style="color:#555">(no task)</i>{{end}}</div>

  <div class="panel">
    <h2>Snapshot</h2>
    {{if .RootCommit}}
      <div class="meta"><b>root:</b> <span class="hash">{{.RootCommit}}</span></div>
      <div class="meta"><b>ref:</b> refs/sessions/{{.ID}}</div>
    {{else}}
      <div class="meta" style="color:#555">no snapshot — session starts empty; first push becomes the root</div>
    {{end}}
    {{if .Result}}<div style="margin-top:8px"><h2 style="margin-bottom:6px">Result</h2><div class="post-content">{{.Result}}</div></div>{{end}}
  </div>

  {{if $.Mutable}}
  <div class="actions">
    {{if eq .Status "open"}}
    <form method="post" action="/admin/sessions/{{.ID}}/close">
      <input type="hidden" name="status" value="done">
      <input type="text" name="commit" placeholder="final commit (optional)">
      <input type="text" name="summary" placeholder="summary…">
      <button class="btn ok" type="submit">mark done</button>
    </form>
    <form method="post" action="/admin/sessions/{{.ID}}/close">
      <input type="hidden" name="status" value="failed">
      <button class="btn warn" type="submit">mark failed</button>
    </form>
    {{end}}
    <form method="post" action="/admin/sessions/{{.ID}}/delete"
          onsubmit="return confirm('Delete session {{.ID}}? This removes all its agents, commits, posts, and snapshot ref. Cannot be undone.');">
      <button class="btn warn" type="submit">delete session</button>
    </form>
  </div>
  {{end}}

  <div class="stats">
    <div class="stat"><div class="stat-value">{{.AgentCount}}</div><div class="stat-label">Agents</div></div>
    <div class="stat"><div class="stat-value">{{.CommitCount}}</div><div class="stat-label">Commits</div></div>
    <div class="stat"><div class="stat-value">{{.PostCount}}</div><div class="stat-label">Posts</div></div>
  </div>
  {{end}}

  <div class="panel">
    <h2>Agents</h2>
    {{if .Agents}}
      <table>
        <tr><th>ID</th><th>Joined</th></tr>
        {{range .Agents}}<tr><td class="agent">{{.ID}}</td><td class="time">{{timeago .CreatedAt}}</td></tr>{{end}}
      </table>
    {{else}}<div class="empty" style="padding:8px 0">no agents in this session</div>{{end}}
  </div>

  <div class="panel">
    <h2>Commits</h2>
    {{if .Commits}}
      <table>
        <tr><th>Hash</th><th>Parent</th><th>Agent</th><th>Message</th><th>When</th></tr>
        {{range .Commits}}
        <tr>
          <td class="hash">{{short .Hash}}</td>
          <td class="time">{{if .ParentHash}}{{short .ParentHash}}{{else}}&mdash;{{end}}</td>
          <td class="agent">{{if .AgentID}}{{.AgentID}}{{else}}<span style="color:#555">(seed)</span>{{end}}</td>
          <td class="msg">{{.Message}}</td>
          <td class="time">{{timeago .CreatedAt}}</td>
        </tr>
        {{end}}
      </table>
    {{else}}<div class="empty" style="padding:8px 0">no commits yet — push from a session agent to seed the work</div>{{end}}
  </div>

  <div class="panel">
    <h2>Board</h2>
    {{if .Posts}}
      {{range .Posts}}
      <div class="post">
        <div class="post-header">
          <span class="channel-tag">#{{.ChannelName}}</span>
          <span class="agent">{{.AgentID}}</span>
          <span class="time">{{timeago .CreatedAt}}</span>
          {{if .ParentID}}<span class="reply-indicator">↳ reply</span>{{end}}
        </div>
        <div class="post-content">{{.Content}}</div>
      </div>
      {{end}}
    {{else}}<div class="empty" style="padding:8px 0">no posts on this session's board yet</div>{{end}}
  </div>

{{else}}
  <div class="stats">
    <div class="stat"><div class="stat-value">{{.Stats.OpenSessions}}/{{.Stats.SessionCount}}</div><div class="stat-label">Sessions (open)</div></div>
    <div class="stat"><div class="stat-value">{{.Stats.AgentCount}}</div><div class="stat-label">Agents</div></div>
    <div class="stat"><div class="stat-value">{{.Stats.CommitCount}}</div><div class="stat-label">Commits</div></div>
    <div class="stat"><div class="stat-value">{{.Stats.PostCount}}</div><div class="stat-label">Posts</div></div>
  </div>
  <div class="empty-state">
    <h3>{{if .Sessions}}select a session from the left{{else}}no sessions yet{{end}}</h3>
    <div>{{if .Mutable}}create one with the form on the sidebar, or via <code>ah session create</code>.{{else}}create one via <code>ah session create</code>.{{end}}</div>
  </div>
{{end}}
</main>

</body>
</html>`))
