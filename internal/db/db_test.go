package db

import (
	"path/filepath"
	"testing"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := d.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// defaultProjectID returns the id of the bootstrapped default project, which
// Migrate creates and into which sessions/channels land when unscoped.
func defaultProjectID(t *testing.T, d *DB) int {
	t.Helper()
	p, err := d.GetProjectBySlug(DefaultProjectSlug)
	if err != nil || p == nil {
		t.Fatalf("default project missing: %v", err)
	}
	return p.ID
}

func TestSessionLifecycle(t *testing.T) {
	d := newTestDB(t)

	if err := d.CreateSession("s1", "do the thing", "", defaultProjectID(t, d)); err != nil {
		t.Fatalf("create: %v", err)
	}
	s, err := d.GetSession("s1")
	if err != nil || s == nil {
		t.Fatalf("get: %v %v", s, err)
	}
	if s.Status != "open" || s.Task != "do the thing" {
		t.Fatalf("unexpected session: %+v", s)
	}
	if s.ClosedAt != nil {
		t.Fatalf("new session should not have closed_at")
	}

	if err := d.CloseSession("s1", "done", "commit=abc"); err != nil {
		t.Fatalf("close: %v", err)
	}
	s, _ = d.GetSession("s1")
	if s.Status != "done" || s.Result != "commit=abc" || s.ClosedAt == nil {
		t.Fatalf("close not applied: %+v", s)
	}

	// Double-close must fail (already closed).
	if err := d.CloseSession("s1", "failed", ""); err == nil {
		t.Fatalf("expected error closing an already-closed session")
	}
	// Closing a non-existent session must fail.
	if err := d.CloseSession("nope", "done", ""); err == nil {
		t.Fatalf("expected error closing unknown session")
	}

	// Missing session reads nil, not error.
	if s, err := d.GetSession("ghost"); err != nil || s != nil {
		t.Fatalf("expected (nil,nil) for missing session, got %v %v", s, err)
	}
}

func TestAgentSessionBinding(t *testing.T) {
	d := newTestDB(t)
	mustSession(t, d, "s1")
	mustSession(t, d, "s2")

	if err := d.CreateAgent("a1", "key1", "s1"); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := d.CreateAgent("a2", "key2", "s1"); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := d.CreateAgent("a3", "key3", "s2"); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	a, err := d.GetAgentByAPIKey("key1")
	if err != nil || a == nil {
		t.Fatalf("lookup: %v %v", a, err)
	}
	if a.SessionID != "s1" {
		t.Fatalf("expected agent bound to s1, got %q", a.SessionID)
	}

	if n, _ := d.CountAgentsInSession("s1"); n != 2 {
		t.Fatalf("expected 2 agents in s1, got %d", n)
	}
	if n, _ := d.CountAgentsInSession("s2"); n != 1 {
		t.Fatalf("expected 1 agent in s2, got %d", n)
	}
	if n, _ := d.CountAgentsInSession("nobody"); n != 0 {
		t.Fatalf("expected 0 agents for unknown session, got %d", n)
	}
}

func TestLeavesSessionIsolation(t *testing.T) {
	d := newTestDB(t)
	mustSession(t, d, "A")
	mustSession(t, d, "B")

	// A: root -> a1 -> a2 ; B: b1 (independent)
	mustCommit(t, d, "root", "", "", "A", "root")
	mustCommit(t, d, "a1", "root", "", "A", "a1")
	mustCommit(t, d, "a2", "a1", "", "A", "a2")
	mustCommit(t, d, "b1", "", "", "B", "b1")

	leavesA, err := d.GetLeaves("A", "")
	if err != nil {
		t.Fatalf("leaves A: %v", err)
	}
	if got := hashes(leavesA); !sameSet(got, []string{"a2"}) {
		t.Fatalf("session A frontier should be [a2], got %v", got)
	}

	leavesB, _ := d.GetLeaves("B", "")
	if got := hashes(leavesB); !sameSet(got, []string{"b1"}) {
		t.Fatalf("session B frontier should be [b1], got %v", got)
	}

	// Global view (operator/dashboard) sees every tip.
	all, _ := d.GetLeaves("", "")
	if got := hashes(all); !sameSet(got, []string{"a2", "b1"}) {
		t.Fatalf("global frontier should be [a2 b1], got %v", got)
	}
}

func TestSnapshotSurfacesThenAdvances(t *testing.T) {
	d := newTestDB(t)
	mustSession(t, d, "seed")
	mustSession(t, d, "work")

	// A baseline commit owned by some other session (the snapshot source).
	mustCommit(t, d, "base", "", "", "seed", "baseline")

	// Fresh "work" session has no commits of its own; its frontier must be
	// the snapshot root even though that commit row belongs to "seed".
	leaves, _ := d.GetLeaves("work", "base")
	if got := hashes(leaves); !sameSet(got, []string{"base"}) {
		t.Fatalf("new session frontier should be the snapshot [base], got %v", got)
	}

	// Swarm builds on the snapshot: the new commit is owned by "work".
	mustCommit(t, d, "w1", "base", "", "work", "change on snapshot")

	leaves, _ = d.GetLeaves("work", "base")
	if got := hashes(leaves); !sameSet(got, []string{"w1"}) {
		t.Fatalf("after building on snapshot frontier should be [w1], got %v", got)
	}

	// The snapshot did not leak into any other session's frontier.
	if l, _ := d.GetLeaves("seed", ""); !sameSet(hashes(l), []string{"base"}) {
		t.Fatalf("seed session frontier should still be [base], got %v", hashes(l))
	}
}

func TestPostsSessionIsolation(t *testing.T) {
	d := newTestDB(t)
	mustSession(t, d, "A")
	mustSession(t, d, "B")
	pid := defaultProjectID(t, d)
	if err := d.CreateChannel(pid, "general", ""); err != nil {
		t.Fatalf("channel: %v", err)
	}
	ch, _ := d.GetChannelByName(pid, "general")
	if err := d.CreateAgent("alice", "ka", "A"); err != nil {
		t.Fatalf("agent alice: %v", err)
	}
	if err := d.CreateAgent("bob", "kb", "B"); err != nil {
		t.Fatalf("agent bob: %v", err)
	}

	if _, err := d.CreatePost(ch.ID, "alice", "A", nil, "from A"); err != nil {
		t.Fatalf("post A: %v", err)
	}
	if _, err := d.CreatePost(ch.ID, "bob", "B", nil, "from B"); err != nil {
		t.Fatalf("post B: %v", err)
	}

	a, _ := d.ListPosts(ch.ID, "A", 0, 0)
	if len(a) != 1 || a[0].Content != "from A" {
		t.Fatalf("session A should see only its post, got %+v", a)
	}
	b, _ := d.ListPosts(ch.ID, "B", 0, 0)
	if len(b) != 1 || b[0].Content != "from B" {
		t.Fatalf("session B should see only its post, got %+v", b)
	}
	all, _ := d.ListPosts(ch.ID, "", 0, 0)
	if len(all) != 2 {
		t.Fatalf("unscoped listing should see both posts, got %d", len(all))
	}
}

// helpers

func mustSession(t *testing.T, d *DB, id string) {
	t.Helper()
	if err := d.CreateSession(id, "task "+id, "", defaultProjectID(t, d)); err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

func mustCommit(t *testing.T, d *DB, hash, parent, agent, session, msg string) {
	t.Helper()
	if err := d.InsertCommit(hash, parent, agent, session, msg); err != nil {
		t.Fatalf("insert commit %s: %v", hash, err)
	}
}

func hashes(cs []Commit) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Hash)
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}
