package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CLIConfig is stored in ~/.agenthub/config.json
type CLIConfig struct {
	ServerURL string `json:"server_url"`
	APIKey    string `json:"api_key"`
	AgentID   string `json:"agent_id"`
	SessionID string `json:"session_id"`
}

func configDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agenthub")
}

func configPath() string {
	return filepath.Join(configDir(), "config.json")
}

func loadConfig() (*CLIConfig, error) {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return nil, fmt.Errorf("no config found — run 'ah join' first")
	}
	var cfg CLIConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}

func saveConfig(cfg *CLIConfig) error {
	os.MkdirAll(configDir(), 0700)
	data, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(configPath(), data, 0600)
}

// HTTP client

type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func newClient(cfg *CLIConfig) *Client {
	return &Client{
		BaseURL: strings.TrimRight(cfg.ServerURL, "/"),
		APIKey:  cfg.APIKey,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
	}
}

func (c *Client) get(path string) (*http.Response, error) {
	req, err := http.NewRequest("GET", c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	return c.HTTP.Do(req)
}

func (c *Client) postJSON(path string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", c.BaseURL+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	return c.HTTP.Do(req)
}

func (c *Client) postFile(path string, filePath string) (*http.Response, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	req, err := http.NewRequest("POST", c.BaseURL+path, f)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/octet-stream")
	return c.HTTP.Do(req)
}

func readJSON(resp *http.Response, v any) error {
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server error %d: %s", resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func readBody(resp *http.Response) (string, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("server error %d: %s", resp.StatusCode, string(body))
	}
	return string(body), nil
}

// Commands

func cmdJoin(args []string) {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	serverFlag := fs.String("server", "", "server URL")
	agentID := fs.String("name", "", "agent name/id")
	adminKey := fs.String("admin-key", "", "admin key to register agent")
	sessionID := fs.String("session", "", "session id to bind this agent to")
	fs.Parse(args)

	// Accept server URL as flag or positional arg
	serverURL := *serverFlag
	if serverURL == "" && fs.NArg() > 0 {
		serverURL = fs.Arg(0)
	}
	serverURL = strings.TrimRight(serverURL, "/")

	if serverURL == "" || *agentID == "" || *adminKey == "" || *sessionID == "" {
		fmt.Fprintln(os.Stderr, "usage: ah join --server <url> --name <id> --admin-key <key> --session <session-id>")
		os.Exit(1)
	}

	// Register agent via admin API
	client := &Client{
		BaseURL: serverURL,
		APIKey:  *adminKey,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
	resp, err := client.postJSON("/api/admin/agents", map[string]string{
		"id":         *agentID,
		"session_id": *sessionID,
	})
	if err != nil {
		fatal("failed to register: %v", err)
	}
	var result map[string]string
	if err := readJSON(resp, &result); err != nil {
		fatal("registration failed: %v", err)
	}

	apiKey := result["api_key"]
	cfg := &CLIConfig{
		ServerURL: serverURL,
		APIKey:    apiKey,
		AgentID:   *agentID,
		SessionID: *sessionID,
	}
	if err := saveConfig(cfg); err != nil {
		fatal("failed to save config: %v", err)
	}

	fmt.Printf("joined %s as %q (session %s)\n", serverURL, *agentID, *sessionID)
	fmt.Printf("api key: %s\n", apiKey)
	fmt.Printf("config saved to %s\n", configPath())
}

func cmdPush(args []string) {
	cfg := mustLoadConfig()
	client := newClient(cfg)

	// Create a bundle from HEAD
	tmpFile, err := os.CreateTemp("", "ah-push-*.bundle")
	if err != nil {
		fatal("create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	// Get current HEAD hash
	headHash, err := gitOutput("rev-parse", "HEAD")
	if err != nil {
		fatal("not in a git repo or no commits: %v", err)
	}
	headHash = strings.TrimSpace(headHash)

	// Create bundle
	if err := gitRun("bundle", "create", tmpFile.Name(), "HEAD"); err != nil {
		fatal("create bundle: %v", err)
	}

	// Upload
	resp, err := client.postFile("/api/git/push", tmpFile.Name())
	if err != nil {
		fatal("push failed: %v", err)
	}
	var result map[string]any
	if err := readJSON(resp, &result); err != nil {
		fatal("push failed: %v", err)
	}

	fmt.Printf("pushed %s\n", headHash[:12])
	if hashes, ok := result["hashes"].([]any); ok {
		for _, h := range hashes {
			fmt.Printf("  indexed: %v\n", h)
		}
	}
}

func cmdFetch(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: ah fetch <hash>")
		os.Exit(1)
	}
	hash := args[0]
	cfg := mustLoadConfig()
	client := newClient(cfg)

	resp, err := client.get("/api/git/fetch/" + hash)
	if err != nil {
		fatal("fetch failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		fatal("fetch failed: %s", string(body))
	}

	// Save to temp file
	tmpFile, err := os.CreateTemp("", "ah-fetch-*.bundle")
	if err != nil {
		fatal("create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		tmpFile.Close()
		fatal("download failed: %v", err)
	}
	tmpFile.Close()

	// Unbundle into local repo
	if err := gitRun("bundle", "unbundle", tmpFile.Name()); err != nil {
		fatal("unbundle failed: %v", err)
	}

	fmt.Printf("fetched %s\n", hash)
}

func cmdLog(args []string) {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	agent := fs.String("agent", "", "filter by agent")
	limit := fs.Int("limit", 20, "max results")
	fs.Parse(args)

	cfg := mustLoadConfig()
	client := newClient(cfg)

	path := fmt.Sprintf("/api/git/commits?limit=%d", *limit)
	if *agent != "" {
		path += "&agent=" + *agent
	}

	resp, err := client.get(path)
	if err != nil {
		fatal("request failed: %v", err)
	}

	var commits []map[string]any
	if err := readJSON(resp, &commits); err != nil {
		fatal("failed: %v", err)
	}

	for _, c := range commits {
		hash := str(c["hash"])
		short := hash
		if len(hash) > 12 {
			short = hash[:12]
		}
		agent := str(c["agent_id"])
		msg := str(c["message"])
		ts := str(c["created_at"])
		if agent == "" {
			agent = "(seed)"
		}
		fmt.Printf("%s  %-12s  %s  %s\n", short, agent, ts[:min(19, len(ts))], msg)
	}
}

func cmdChildren(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: ah children <hash>")
		os.Exit(1)
	}
	cfg := mustLoadConfig()
	client := newClient(cfg)

	resp, err := client.get("/api/git/commits/" + args[0] + "/children")
	if err != nil {
		fatal("request failed: %v", err)
	}
	printCommitList(resp)
}

func cmdLeaves(args []string) {
	cfg := mustLoadConfig()
	client := newClient(cfg)

	resp, err := client.get("/api/git/leaves")
	if err != nil {
		fatal("request failed: %v", err)
	}
	printCommitList(resp)
}

func cmdLineage(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: ah lineage <hash>")
		os.Exit(1)
	}
	cfg := mustLoadConfig()
	client := newClient(cfg)

	resp, err := client.get("/api/git/commits/" + args[0] + "/lineage")
	if err != nil {
		fatal("request failed: %v", err)
	}
	printCommitList(resp)
}

func cmdDiff(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ah diff <hash-a> <hash-b>")
		os.Exit(1)
	}
	cfg := mustLoadConfig()
	client := newClient(cfg)

	resp, err := client.get("/api/git/diff/" + args[0] + "/" + args[1])
	if err != nil {
		fatal("request failed: %v", err)
	}
	body, err := readBody(resp)
	if err != nil {
		fatal("diff failed: %v", err)
	}
	fmt.Print(body)
}

func cmdChannels(args []string) {
	cfg := mustLoadConfig()
	client := newClient(cfg)

	resp, err := client.get("/api/channels")
	if err != nil {
		fatal("request failed: %v", err)
	}

	var channels []map[string]any
	if err := readJSON(resp, &channels); err != nil {
		fatal("failed: %v", err)
	}

	if len(channels) == 0 {
		fmt.Println("no channels")
		return
	}
	for _, ch := range channels {
		desc := str(ch["description"])
		if desc != "" {
			desc = " — " + desc
		}
		fmt.Printf("#%-20s%s\n", str(ch["name"]), desc)
	}
}

func cmdPost(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ah post <channel> <message>")
		os.Exit(1)
	}
	channel := args[0]
	message := strings.Join(args[1:], " ")

	cfg := mustLoadConfig()
	client := newClient(cfg)

	resp, err := client.postJSON("/api/channels/"+channel+"/posts", map[string]any{
		"content": message,
	})
	if err != nil {
		fatal("post failed: %v", err)
	}
	var post map[string]any
	if err := readJSON(resp, &post); err != nil {
		fatal("post failed: %v", err)
	}
	fmt.Printf("posted #%v in #%s\n", post["id"], channel)
}

func cmdRead(args []string) {
	fs := flag.NewFlagSet("read", flag.ExitOnError)
	limit := fs.Int("limit", 20, "max posts")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: ah read <channel> [--limit N]")
		os.Exit(1)
	}
	channel := fs.Arg(0)

	cfg := mustLoadConfig()
	client := newClient(cfg)

	resp, err := client.get(fmt.Sprintf("/api/channels/%s/posts?limit=%d", channel, *limit))
	if err != nil {
		fatal("request failed: %v", err)
	}

	var posts []map[string]any
	if err := readJSON(resp, &posts); err != nil {
		fatal("failed: %v", err)
	}

	if len(posts) == 0 {
		fmt.Printf("#%s is empty\n", channel)
		return
	}

	// Print in chronological order (server returns DESC)
	for i := len(posts) - 1; i >= 0; i-- {
		p := posts[i]
		id := fmt.Sprintf("%v", p["id"])
		agent := str(p["agent_id"])
		content := str(p["content"])
		ts := str(p["created_at"])
		parentID := p["parent_id"]

		prefix := ""
		if parentID != nil {
			prefix = fmt.Sprintf("  ↳ reply to #%v | ", parentID)
		}
		fmt.Printf("[%s] %s%s (%s): %s\n", id, prefix, agent, ts[:min(19, len(ts))], content)
	}
}

func cmdReply(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ah reply <post-id> <message>")
		os.Exit(1)
	}
	postID, err := strconv.Atoi(args[0])
	if err != nil {
		fatal("invalid post id: %s", args[0])
	}
	message := strings.Join(args[1:], " ")

	cfg := mustLoadConfig()
	client := newClient(cfg)

	// Get the post to find its channel
	resp, err := client.get(fmt.Sprintf("/api/posts/%d", postID))
	if err != nil {
		fatal("request failed: %v", err)
	}
	var post map[string]any
	if err := readJSON(resp, &post); err != nil {
		fatal("post not found: %v", err)
	}

	// Get channel name from channel_id
	channelID := int(post["channel_id"].(float64))
	// We need the channel name — list channels and find it
	resp2, err := client.get("/api/channels")
	if err != nil {
		fatal("request failed: %v", err)
	}
	var channels []map[string]any
	if err := readJSON(resp2, &channels); err != nil {
		fatal("failed: %v", err)
	}
	var channelName string
	for _, ch := range channels {
		if int(ch["id"].(float64)) == channelID {
			channelName = str(ch["name"])
			break
		}
	}
	if channelName == "" {
		fatal("could not find channel for post %d", postID)
	}

	resp3, err := client.postJSON("/api/channels/"+channelName+"/posts", map[string]any{
		"content":   message,
		"parent_id": postID,
	})
	if err != nil {
		fatal("reply failed: %v", err)
	}
	var result map[string]any
	if err := readJSON(resp3, &result); err != nil {
		fatal("reply failed: %v", err)
	}
	fmt.Printf("replied #%v to #%d in #%s\n", result["id"], postID, channelName)
}

// Session commands (operator-owned lifecycle)

func cmdSession(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: ah session <create|list|close|show> ...")
		os.Exit(1)
	}
	switch args[0] {
	case "create":
		cmdSessionCreate(args[1:])
	case "list":
		cmdSessionList(args[1:])
	case "close":
		cmdSessionClose(args[1:])
	case "delete":
		cmdSessionDelete(args[1:])
	case "show":
		cmdSessionShow(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown session subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func adminClient(server, adminKey string) *Client {
	if server == "" || adminKey == "" {
		fmt.Fprintln(os.Stderr, "--server and --admin-key are required")
		os.Exit(1)
	}
	return &Client{
		BaseURL: strings.TrimRight(server, "/"),
		APIKey:  adminKey,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

func cmdSessionCreate(args []string) {
	fs := flag.NewFlagSet("session create", flag.ExitOnError)
	server := fs.String("server", "", "server URL")
	adminKey := fs.String("admin-key", "", "admin key")
	task := fs.String("task", "", "task description for the swarm")
	id := fs.String("id", "", "optional explicit session id")
	base := fs.String("base", "", "commit hash to snapshot (optional; omitted = session starts empty)")
	fs.Parse(args)

	if *task == "" {
		fatal("--task is required")
	}
	client := adminClient(*server, *adminKey)
	resp, err := client.postJSON("/api/admin/sessions", map[string]string{"id": *id, "task": *task, "base": *base})
	if err != nil {
		fatal("create failed: %v", err)
	}
	var sess map[string]any
	if err := readJSON(resp, &sess); err != nil {
		fatal("create failed: %v", err)
	}
	fmt.Printf("session %v created (status=%v)\n", sess["id"], sess["status"])
	fmt.Printf("task: %v\n", sess["task"])
	if rc := str(sess["root_commit"]); rc != "" {
		fmt.Printf("snapshot: %s (frozen at refs/sessions/%v)\n", rc, sess["id"])
	} else {
		fmt.Println("snapshot: (none — no --base given; first push becomes the root)")
	}
	fmt.Printf("\nprovision agents with:\n  ah join --server %s --name <id> --admin-key <key> --session %v\n",
		strings.TrimRight(*server, "/"), sess["id"])
}

func cmdSessionList(args []string) {
	fs := flag.NewFlagSet("session list", flag.ExitOnError)
	server := fs.String("server", "", "server URL")
	adminKey := fs.String("admin-key", "", "admin key")
	fs.Parse(args)

	client := adminClient(*server, *adminKey)
	resp, err := client.get("/api/admin/sessions")
	if err != nil {
		fatal("request failed: %v", err)
	}
	var sessions []map[string]any
	if err := readJSON(resp, &sessions); err != nil {
		fatal("failed: %v", err)
	}
	if len(sessions) == 0 {
		fmt.Println("no sessions")
		return
	}
	for _, s := range sessions {
		fmt.Printf("%-16v %-7v agents=%v commits=%v posts=%v  %s\n",
			s["id"], s["status"], s["AgentCount"], s["CommitCount"], s["PostCount"], str(s["task"]))
	}
}

func cmdSessionClose(args []string) {
	fs := flag.NewFlagSet("session close", flag.ExitOnError)
	server := fs.String("server", "", "server URL")
	adminKey := fs.String("admin-key", "", "admin key")
	status := fs.String("status", "done", "done | failed")
	commit := fs.String("result", "", "final result commit hash")
	summary := fs.String("summary", "", "result summary")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fatal("usage: ah session close <session-id> [--status done|failed] [--result <hash>] [--summary ...]")
	}
	client := adminClient(*server, *adminKey)
	resp, err := client.postJSON("/api/admin/sessions/"+fs.Arg(0)+"/close", map[string]string{
		"status":  *status,
		"commit":  *commit,
		"summary": *summary,
	})
	if err != nil {
		fatal("close failed: %v", err)
	}
	var sess map[string]any
	if err := readJSON(resp, &sess); err != nil {
		fatal("close failed: %v", err)
	}
	fmt.Printf("session %v closed (status=%v)\n", sess["id"], sess["status"])
}

func cmdSessionDelete(args []string) {
	fs := flag.NewFlagSet("session delete", flag.ExitOnError)
	server := fs.String("server", "", "server URL")
	adminKey := fs.String("admin-key", "", "admin key (omit when targeting a --no-auth server)")
	yes := fs.Bool("yes", false, "skip confirmation")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fatal("usage: ah session delete <session-id> [--yes]")
	}
	id := fs.Arg(0)
	if !*yes {
		fmt.Fprintf(os.Stderr, "delete session %s and all its agents/commits/posts? [y/N]: ", id)
		var ans string
		fmt.Scanln(&ans)
		if ans != "y" && ans != "Y" {
			fmt.Println("aborted")
			return
		}
	}
	client := &Client{
		BaseURL: strings.TrimRight(*server, "/"),
		APIKey:  *adminKey,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
	req, err := http.NewRequest("DELETE", client.BaseURL+"/api/admin/sessions/"+id, nil)
	if err != nil {
		fatal("request: %v", err)
	}
	if client.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+client.APIKey)
	}
	resp, err := client.HTTP.Do(req)
	if err != nil {
		fatal("delete failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		fatal("delete failed: %d %s", resp.StatusCode, string(body))
	}
	fmt.Printf("session %s deleted\n", id)
}

func cmdSessionShow(args []string) {
	cfg := mustLoadConfig()
	client := newClient(cfg)
	resp, err := client.get("/api/session")
	if err != nil {
		fatal("request failed: %v", err)
	}
	var sess map[string]any
	if err := readJSON(resp, &sess); err != nil {
		fatal("failed: %v", err)
	}
	fmt.Printf("session:  %v\n", sess["id"])
	fmt.Printf("status:   %v\n", sess["status"])
	fmt.Printf("task:     %v\n", sess["task"])
	if rc := str(sess["root_commit"]); rc != "" {
		fmt.Printf("snapshot: %s\n", rc)
	}
	if r := str(sess["result"]); r != "" {
		fmt.Printf("result:   %s\n", r)
	}
}

// Helpers

func mustLoadConfig() *CLIConfig {
	cfg, err := loadConfig()
	if err != nil {
		fatal("%v", err)
	}
	return cfg
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

func gitRun(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func gitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	return string(out), err
}

func printCommitList(resp *http.Response) {
	var commits []map[string]any
	if err := readJSON(resp, &commits); err != nil {
		fatal("failed: %v", err)
	}
	if len(commits) == 0 {
		fmt.Println("(none)")
		return
	}
	for _, c := range commits {
		hash := str(c["hash"])
		short := hash
		if len(hash) > 12 {
			short = hash[:12]
		}
		agent := str(c["agent_id"])
		msg := str(c["message"])
		if agent == "" {
			agent = "(seed)"
		}
		fmt.Printf("%s  %-12s  %s\n", short, agent, msg)
	}
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "join":
		cmdJoin(args)
	case "session":
		cmdSession(args)
	case "push":
		cmdPush(args)
	case "fetch":
		cmdFetch(args)
	case "log":
		cmdLog(args)
	case "children":
		cmdChildren(args)
	case "leaves":
		cmdLeaves(args)
	case "lineage":
		cmdLineage(args)
	case "diff":
		cmdDiff(args)
	case "channels":
		cmdChannels(args)
	case "post":
		cmdPost(args)
	case "read":
		cmdRead(args)
	case "reply":
		cmdReply(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`ah — CLI for Agent Hub

Session commands (operator):
  session create --task "..." --server <url> --admin-key <key>
  session list --server <url> --admin-key <key>
  session close <id> [--status done|failed] [--result <hash>] [--summary ...]
  session delete <id> [--yes]                 remove session + its agents/commits/posts
  session show                                show this agent's session

Git commands:
  join <url> --name <id> --admin-key <key> --session <id>   register as agent
  push                                        push HEAD commit to hub
  fetch <hash>                                fetch a commit from hub
  log [--agent X] [--limit N]                 list recent commits
  children <hash>                             children of a commit
  leaves                                      frontier commits
  lineage <hash>                              ancestry to root
  diff <hash-a> <hash-b>                      diff two commits

Board commands:
  channels                                    list channels
  post <channel> <message>                    post to a channel
  read <channel> [--limit N]                  read channel posts
  reply <post-id> <message>                   reply to a post`)
}
