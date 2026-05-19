package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"agenthub/internal/db"
	"agenthub/internal/gitrepo"
)

func newTestServer(t *testing.T, cfg Config) (*httptest.Server, *db.DB) {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := database.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo, err := gitrepo.Init(filepath.Join(dir, "repo.git"))
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	srv := New(database, repo, "admin", cfg)
	ts := httptest.NewServer(srv.mux)
	t.Cleanup(func() { ts.Close(); database.Close() })
	return ts, database
}

func do(t *testing.T, method, url, bearer string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rdr)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAgentCapEnforced(t *testing.T) {
	ts, _ := newTestServer(t, Config{MaxAgentsPerSession: 2})

	code, sess := do(t, "POST", ts.URL+"/api/admin/sessions", "admin", map[string]string{"task": "x"})
	if code != http.StatusCreated {
		t.Fatalf("create session: %d %v", code, sess)
	}
	sid := sess["id"].(string)

	for i, name := range []string{"w1", "w2"} {
		code, body := do(t, "POST", ts.URL+"/api/admin/agents", "admin",
			map[string]string{"id": name, "session_id": sid})
		if code != http.StatusCreated {
			t.Fatalf("agent %d (%s) should succeed, got %d %v", i, name, code, body)
		}
	}
	// Third exceeds the cap.
	code, body := do(t, "POST", ts.URL+"/api/admin/agents", "admin",
		map[string]string{"id": "w3", "session_id": sid})
	if code != http.StatusConflict {
		t.Fatalf("3rd agent should be 409 (cap=2), got %d %v", code, body)
	}
}

func TestUnlimitedAgentsWhenCapZero(t *testing.T) {
	ts, _ := newTestServer(t, Config{MaxAgentsPerSession: 0})
	_, sess := do(t, "POST", ts.URL+"/api/admin/sessions", "admin", map[string]string{"task": "x"})
	sid := sess["id"].(string)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		code, body := do(t, "POST", ts.URL+"/api/admin/agents", "admin",
			map[string]string{"id": name, "session_id": sid})
		if code != http.StatusCreated {
			t.Fatalf("agent %s should succeed with no cap, got %d %v", name, code, body)
		}
	}
}

func TestClosedSessionRejectsWrites(t *testing.T) {
	ts, _ := newTestServer(t, Config{MaxPostsPerHour: 100, MaxPushesPerHour: 100})

	_, sess := do(t, "POST", ts.URL+"/api/admin/sessions", "admin", map[string]string{"task": "x"})
	sid := sess["id"].(string)
	_, ag := do(t, "POST", ts.URL+"/api/admin/agents", "admin",
		map[string]string{"id": "w1", "session_id": sid})
	key := ag["api_key"].(string)

	// Channel + post works while open.
	do(t, "POST", ts.URL+"/api/channels", key, map[string]string{"name": "general"})
	if code, b := do(t, "POST", ts.URL+"/api/channels/general/posts", key,
		map[string]string{"content": "hi"}); code != http.StatusCreated {
		t.Fatalf("post while open should succeed, got %d %v", code, b)
	}

	// Close it.
	if code, b := do(t, "POST", ts.URL+"/api/admin/sessions/"+sid+"/close", "admin",
		map[string]string{"status": "done"}); code != http.StatusOK {
		t.Fatalf("close should succeed, got %d %v", code, b)
	}

	// Writes now rejected, reads still allowed.
	if code, _ := do(t, "POST", ts.URL+"/api/channels/general/posts", key,
		map[string]string{"content": "late"}); code != http.StatusConflict {
		t.Fatalf("post after close should be 409, got %d", code)
	}
	if code, _ := do(t, "GET", ts.URL+"/api/channels/general/posts", key, nil); code != http.StatusOK {
		t.Fatalf("read after close should still be 200 (archive), got %d", code)
	}
	// Can't add agents to a closed session.
	if code, _ := do(t, "POST", ts.URL+"/api/admin/agents", "admin",
		map[string]string{"id": "late", "session_id": sid}); code != http.StatusConflict {
		t.Fatalf("adding agent to closed session should be 409, got %d", code)
	}
}

func TestUnboundAgentDeniedScopedReads(t *testing.T) {
	ts, database := newTestServer(t, Config{})
	// An agent with no session (e.g. a legacy row) must not get the
	// unscoped global view.
	if err := database.CreateAgent("legacy", "legacykey", ""); err != nil {
		t.Fatalf("create unbound agent: %v", err)
	}
	for _, path := range []string{"/api/git/leaves", "/api/git/commits", "/api/channels/general/posts"} {
		if code, _ := do(t, "GET", ts.URL+path, "legacykey", nil); code != http.StatusForbidden {
			t.Fatalf("unbound agent GET %s should be 403, got %d", path, code)
		}
	}
}

func TestPostsNotEnumerableAcrossSessions(t *testing.T) {
	ts, _ := newTestServer(t, Config{MaxPostsPerHour: 100})

	_, sA := do(t, "POST", ts.URL+"/api/admin/sessions", "admin", map[string]string{"task": "A"})
	_, agA := do(t, "POST", ts.URL+"/api/admin/agents", "admin",
		map[string]string{"id": "a", "session_id": sA["id"].(string)})
	keyA := agA["api_key"].(string)

	_, sB := do(t, "POST", ts.URL+"/api/admin/sessions", "admin", map[string]string{"task": "B"})
	_, agB := do(t, "POST", ts.URL+"/api/admin/agents", "admin",
		map[string]string{"id": "b", "session_id": sB["id"].(string)})
	keyB := agB["api_key"].(string)

	do(t, "POST", ts.URL+"/api/channels", keyA, map[string]string{"name": "general"})
	_, post := do(t, "POST", ts.URL+"/api/channels/general/posts", keyA,
		map[string]string{"content": "secret from A"})
	pid := int(post["id"].(float64))

	// A can read its own post.
	if code, _ := do(t, "GET", ts.URL+fmt.Sprintf("/api/posts/%d", pid), keyA, nil); code != http.StatusOK {
		t.Fatalf("A should read its own post, got %d", code)
	}
	// B must not be able to enumerate A's post by id.
	if code, _ := do(t, "GET", ts.URL+fmt.Sprintf("/api/posts/%d", pid), keyB, nil); code != http.StatusNotFound {
		t.Fatalf("B probing A's post id should be 404, got %d", code)
	}
	if code, _ := do(t, "GET", ts.URL+fmt.Sprintf("/api/posts/%d/replies", pid), keyB, nil); code != http.StatusNotFound {
		t.Fatalf("B probing A's replies should be 404, got %d", code)
	}
}

func TestGitReadsScopedToSession(t *testing.T) {
	ts, database := newTestServer(t, Config{})

	_, sA := do(t, "POST", ts.URL+"/api/admin/sessions", "admin", map[string]string{"task": "A"})
	_, agA := do(t, "POST", ts.URL+"/api/admin/agents", "admin",
		map[string]string{"id": "a", "session_id": sA["id"].(string)})
	keyA := agA["api_key"].(string)

	_, sB := do(t, "POST", ts.URL+"/api/admin/sessions", "admin", map[string]string{"task": "B"})
	bID := sB["id"].(string)

	// A commit that exists only in session B.
	bHash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := database.InsertCommit(bHash, "", "", bID, "B private work"); err != nil {
		t.Fatalf("insert B commit: %v", err)
	}

	// Session A must not see B's commit by any read path.
	if code, _ := do(t, "GET", ts.URL+"/api/git/commits/"+bHash, keyA, nil); code != http.StatusNotFound {
		t.Fatalf("A get B commit should be 404, got %d", code)
	}
	if code, _ := do(t, "GET", ts.URL+"/api/git/fetch/"+bHash, keyA, nil); code != http.StatusNotFound {
		t.Fatalf("A fetch B commit should be 404, got %d", code)
	}
	if code, _ := do(t, "GET", ts.URL+"/api/git/diff/"+bHash+"/"+bHash, keyA, nil); code != http.StatusNotFound {
		t.Fatalf("A diff B commit should be 404, got %d", code)
	}
	// Lineage of a non-visible commit yields an empty chain, not B's history.
	code, _ := do(t, "GET", ts.URL+"/api/git/commits/"+bHash+"/lineage", keyA, nil)
	if code != http.StatusOK {
		t.Fatalf("lineage should be 200, got %d", code)
	}
}

func TestSessionCreateHasNoImplicitBase(t *testing.T) {
	ts, database := newTestServer(t, Config{})

	_, s1 := do(t, "POST", ts.URL+"/api/admin/sessions", "admin", map[string]string{"task": "first"})
	if rc, ok := s1["root_commit"]; ok && rc != "" {
		t.Fatalf("session created without --base must have no snapshot, got %v", rc)
	}

	// Even with commits already in the hub, a new base-less session must not
	// silently inherit some unrelated tip.
	if err := database.InsertCommit("cccccccccccccccccccccccccccccccccccccccc",
		"", "", s1["id"].(string), "work"); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	_, s2 := do(t, "POST", ts.URL+"/api/admin/sessions", "admin", map[string]string{"task": "second"})
	if rc, ok := s2["root_commit"]; ok && rc != "" {
		t.Fatalf("second base-less session must still have no snapshot, got %v", rc)
	}
}

func TestRegisterRequiresOpenSession(t *testing.T) {
	ts, _ := newTestServer(t, Config{MaxPostsPerHour: 100, MaxPushesPerHour: 100})

	if code, _ := do(t, "POST", ts.URL+"/api/register", "",
		map[string]string{"id": "x"}); code != http.StatusBadRequest {
		t.Fatalf("register without session_id should be 400, got %d", code)
	}
	if code, _ := do(t, "POST", ts.URL+"/api/register", "",
		map[string]string{"id": "x", "session_id": "ghost"}); code != http.StatusBadRequest {
		t.Fatalf("register into unknown session should be 400, got %d", code)
	}
}
