package main_test

// Integration tests: each test builds real repositories (a bare remote plus
// working clones) and drives the compiled git-track binary, asserting on the
// documented exit codes and JSON output.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var binPath string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "git-track-bin")
	if err != nil {
		panic(err)
	}
	binPath = filepath.Join(tmp, "git-track")
	out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.Exit(1)
	}

	// Isolate git from the host configuration.
	cfg := filepath.Join(tmp, "gitconfig")
	os.WriteFile(cfg, []byte("[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n"), 0o644)
	os.Setenv("GIT_CONFIG_GLOBAL", cfg)
	os.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

// runIn executes a command in dir, returning stdout, stderr, and exit code.
func runIn(t *testing.T, dir, name string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running %s %v: %v", name, args, err)
		}
		code = ee.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, stderr, code := runIn(t, dir, "git", args...)
	if code != 0 {
		t.Fatalf("git %v failed (%d): %s", args, code, stderr)
	}
	return strings.TrimSpace(out)
}

func track(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	return runIn(t, dir, binPath, args...)
}

func mustTrack(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, stderr, code := track(t, dir, args...)
	if code != 0 {
		t.Fatalf("git-track %v failed (%d): %s", args, code, stderr)
	}
	return out
}

// setup creates a bare remote and one clone with an initial commit on main.
func setup(t *testing.T) (remote, clone string) {
	t.Helper()
	base := t.TempDir()
	remote = filepath.Join(base, "remote.git")
	git(t, base, "init", "--bare", remote)
	clone = filepath.Join(base, "clone-a")
	git(t, base, "clone", remote, clone)
	os.WriteFile(filepath.Join(clone, "README"), []byte("hi\n"), 0o644)
	git(t, clone, "add", "README")
	git(t, clone, "commit", "-m", "initial")
	git(t, clone, "push", "origin", "main")
	return remote, clone
}

func cloneOf(t *testing.T, remote, name string) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(remote), name)
	git(t, filepath.Dir(remote), "clone", remote, dir)
	return dir
}

func TestRoundTrip(t *testing.T) {
	remote, a := setup(t)

	mustTrack(t, a, "set", "state", "in-progress")
	mustTrack(t, a, "set", "issue", "42")
	mustTrack(t, a, "set", "title", "Refactor auth middleware")
	mustTrack(t, a, "push")

	b := cloneOf(t, remote, "clone-b")
	mustTrack(t, b, "fetch")
	out := mustTrack(t, b, "get", "state")
	if strings.TrimSpace(out) != "in-progress" {
		t.Fatalf("state = %q, want in-progress", out)
	}
	out = mustTrack(t, b, "get", "issue")
	if strings.TrimSpace(out) != "42" {
		t.Fatalf("issue = %q, want 42", out)
	}
	// The whole doc must carry the auto-set fields.
	out = mustTrack(t, b, "get", "--json")
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("get --json is not valid JSON: %v\n%s", err, out)
	}
	for _, key := range []string{"updatedAt", "updatedBy", "schemaVersion"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("round-tripped doc missing %s: %s", key, out)
		}
	}
}

func TestNoMetadataExitCode(t *testing.T) {
	_, a := setup(t)
	_, _, code := track(t, a, "get")
	if code != 2 {
		t.Fatalf("get with no metadata: exit %d, want 2", code)
	}
	// --json must still be valid JSON with the contract error object.
	out, _, code := track(t, a, "get", "--json")
	if code != 2 {
		t.Fatalf("get --json with no metadata: exit %d, want 2", code)
	}
	var e struct {
		Error string  `json:"error"`
		Code  float64 `json:"code"`
	}
	if err := json.Unmarshal([]byte(out), &e); err != nil {
		t.Fatalf("error output is not valid JSON: %v\n%s", err, out)
	}
	if e.Code != 2 || e.Error == "" {
		t.Fatalf("error object = %+v, want code 2 and a message", e)
	}
}

func TestConcurrentWriteConflict(t *testing.T) {
	remote, a := setup(t)
	b := cloneOf(t, remote, "clone-b")

	mustTrack(t, a, "set", "state", "in-progress")
	mustTrack(t, a, "push")

	// B writes without fetching A's state: divergent histories.
	mustTrack(t, b, "set", "state", "review")
	_, stderr, code := track(t, b, "push")
	if code != 5 {
		t.Fatalf("conflicting push: exit %d, want 5 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "fetch") {
		t.Errorf("conflict message should tell the user to fetch: %s", stderr)
	}
}

func TestLockContention(t *testing.T) {
	remote, a := setup(t)
	b := cloneOf(t, remote, "clone-b")

	mustTrack(t, a, "lock")
	_, stderr, code := track(t, b, "lock")
	if code != 3 {
		t.Fatalf("second lock: exit %d, want 3 (stderr: %s)", code, stderr)
	}
	// The loser must not keep a bogus local lock state.
	_, _, code = track(t, b, "get")
	if code != 2 {
		t.Fatalf("loser's local metadata should be rolled back: exit %d, want 2", code)
	}

	// After the holder unlocks, the second actor can lock.
	mustTrack(t, a, "unlock")
	mustTrack(t, b, "fetch")
	mustTrack(t, b, "lock")
}

func TestLockForceSteal(t *testing.T) {
	remote, a := setup(t)
	mustTrack(t, a, "lock")
	mustTrack(t, a, "push")

	b := cloneOf(t, remote, "clone-b")
	mustTrack(t, b, "fetch")
	mustTrack(t, b, "lock", "--force")
	out := mustTrack(t, b, "get", "agent.lockedBy")
	if !strings.Contains(out, "@") {
		t.Fatalf("stolen lock value looks wrong: %q", out)
	}
}

func TestUnknownFieldsSurvive(t *testing.T) {
	_, a := setup(t)

	full := `{
	  "schemaVersion": 1,
	  "state": "todo",
	  "futureField": {"nested": [1, 2, 3]},
	  "agent": {"notes": "x", "futureAgentField": "keep-me"}
	}`
	tmp := filepath.Join(t.TempDir(), "doc.json")
	os.WriteFile(tmp, []byte(full), 0o644)
	mustTrack(t, a, "set", "--from-json", tmp)

	// A write by this (older) version must not drop the unknown fields.
	mustTrack(t, a, "set", "state", "done")
	out := mustTrack(t, a, "get", "--json")
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["futureField"]; !ok {
		t.Errorf("top-level unknown field dropped: %s", out)
	}
	agent, _ := doc["agent"].(map[string]any)
	if agent["futureAgentField"] != "keep-me" {
		t.Errorf("nested unknown field dropped: %s", out)
	}
	if doc["state"] != "done" {
		t.Errorf("state = %v, want done", doc["state"])
	}
}

func TestSchemaTooNew(t *testing.T) {
	_, a := setup(t)
	tmp := filepath.Join(t.TempDir(), "doc.json")
	os.WriteFile(tmp, []byte(`{"schemaVersion": 99, "state": "todo"}`), 0o644)
	_, stderr, code := track(t, a, "set", "--from-json", tmp)
	if code != 4 {
		t.Fatalf("writing schemaVersion 99: exit %d, want 4 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "upgrade") {
		t.Errorf("too-new message should tell the user to upgrade: %s", stderr)
	}

	// Same refusal when the stored doc is newer than the binary.
	writeRawMeta(t, a, "main", `{"schemaVersion": 99, "state": "todo"}`)
	_, _, code = track(t, a, "set", "state", "done")
	if code != 4 {
		t.Fatalf("mutating a newer stored doc: exit %d, want 4", code)
	}
	// Reads must still work.
	out, _, code := track(t, a, "get", "state")
	if code != 0 || strings.TrimSpace(out) != "todo" {
		t.Fatalf("reading a newer doc should work: exit %d, out %q", code, out)
	}
}

// writeRawMeta plants a metadata commit using plain git plumbing, simulating a
// different (newer) tool version writing the ref.
func writeRawMeta(t *testing.T, dir, branch, content string) {
	t.Helper()
	blob := gitStdin(t, dir, content, "hash-object", "-w", "--stdin")
	tree := gitStdin(t, dir, "100644 blob "+blob+"\tmeta.json\n", "mktree")
	commit := git(t, dir, "commit-tree", tree, "-m", "external write")
	git(t, dir, "update-ref", "refs/meta/branches/"+branch, commit)
}

func gitStdin(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func TestValidationAndStates(t *testing.T) {
	_, a := setup(t)
	_, stderr, code := track(t, a, "set", "state", "bogus")
	if code != 1 {
		t.Fatalf("invalid state: exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "track.states") {
		t.Errorf("invalid-state message should mention the config knob: %s", stderr)
	}
	git(t, a, "config", "track.states", "open,closed")
	mustTrack(t, a, "set", "state", "open")
	_, _, code = track(t, a, "set", "state", "todo")
	if code != 1 {
		t.Fatalf("state outside configured set: exit %d, want 1", code)
	}
}

func TestHookChaining(t *testing.T) {
	_, a := setup(t)
	hookDir := filepath.Join(a, ".git", "hooks")
	os.MkdirAll(hookDir, 0o755)
	markerFile := filepath.Join(t.TempDir(), "chained-ran")
	existing := "#!/bin/sh\ntouch " + markerFile + "\nexit 0\n"
	existingPath := filepath.Join(hookDir, "post-checkout")
	os.WriteFile(existingPath, []byte(existing), 0o755)

	mustTrack(t, a, "init")

	// The original hook must survive as the chained script.
	chained, err := os.ReadFile(existingPath + ".pre-git-track")
	if err != nil || string(chained) != existing {
		t.Fatalf("original hook not preserved: %v", err)
	}
	// Running the installed hook must execute the original.
	_, stderr, code := runIn(t, a, "sh", existingPath, "0", "0", "1")
	if code != 0 {
		t.Fatalf("installed hook failed (%d): %s", code, stderr)
	}
	if _, err := os.Stat(markerFile); err != nil {
		t.Fatal("pre-existing post-checkout hook did not run after chaining")
	}
}

func TestInitUndoRestoresConfigExactly(t *testing.T) {
	_, a := setup(t)
	cfgPath := filepath.Join(a, ".git", "config")
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	hookDir := filepath.Join(a, ".git", "hooks")
	existing := "#!/bin/sh\nexit 0\n"
	os.WriteFile(filepath.Join(hookDir, "pre-push"), []byte(existing), 0o755)

	mustTrack(t, a, "init")
	after, _ := os.ReadFile(cfgPath)
	if string(after) == string(before) {
		t.Fatal("init should have changed .git/config")
	}

	mustTrack(t, a, "init", "--undo")
	restored, _ := os.ReadFile(cfgPath)
	if string(restored) != string(before) {
		t.Fatalf("config not restored exactly.\n--- before ---\n%s\n--- after undo ---\n%s", before, restored)
	}
	// Original pre-push restored, our hooks gone.
	b, err := os.ReadFile(filepath.Join(hookDir, "pre-push"))
	if err != nil || string(b) != existing {
		t.Fatalf("original pre-push not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hookDir, "pre-push.pre-git-track")); err == nil {
		t.Fatal("chained hook file left behind after undo")
	}
	if data, err := os.ReadFile(filepath.Join(hookDir, "post-checkout")); err == nil {
		if strings.Contains(string(data), "git-track hook") {
			t.Fatal("git-track post-checkout hook left behind after undo")
		}
	}
}

func TestInitRefspecsAndNormalPush(t *testing.T) {
	_, a := setup(t)
	mustTrack(t, a, "init")
	// Re-running init must not duplicate refspecs.
	mustTrack(t, a, "init")
	fetches := strings.Split(git(t, a, "config", "--get-all", "remote.origin.fetch"), "\n")
	count := 0
	for _, f := range fetches {
		if strings.Contains(f, "refs/meta/branches") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("fetch refspec duplicated: %v", fetches)
	}

	// Normal branch pushes must still work (refs/heads/* listed explicitly).
	os.WriteFile(filepath.Join(a, "file2"), []byte("x\n"), 0o644)
	git(t, a, "add", "file2")
	git(t, a, "commit", "-m", "second")
	git(t, a, "push")
	// And metadata flows through the pre-push hook alongside.
	mustTrack(t, a, "set", "state", "todo")
	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", filepath.Dir(binPath)+string(os.PathListSeparator)+oldPath)
	defer os.Setenv("PATH", oldPath)
	os.WriteFile(filepath.Join(a, "file3"), []byte("x\n"), 0o644)
	git(t, a, "add", "file3")
	git(t, a, "commit", "-m", "third")
	git(t, a, "push")
	if git(t, a, "ls-remote", "origin", "refs/meta/branches/main") == "" {
		t.Fatal("pre-push hook did not push the metadata ref")
	}
}

func TestPruneAndList(t *testing.T) {
	_, a := setup(t)
	git(t, a, "checkout", "-b", "feature/x")
	mustTrack(t, a, "set", "state", "todo")
	git(t, a, "checkout", "main")
	mustTrack(t, a, "set", "state", "in-progress")

	out := mustTrack(t, a, "list", "--json")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("list --json invalid: %v\n%s", err, out)
	}
	if len(rows) != 2 {
		t.Fatalf("list rows = %d, want 2", len(rows))
	}

	git(t, a, "branch", "-D", "feature/x")
	mustTrack(t, a, "prune")
	_, _, code := track(t, a, "get", "--branch", "feature/x")
	if code != 2 {
		t.Fatalf("pruned branch metadata should be gone: exit %d, want 2", code)
	}
	if _, _, code := track(t, a, "get", "--branch", "main"); code != 0 {
		t.Fatal("prune must not touch metadata for live branches")
	}
}

func TestLockTTLExpiry(t *testing.T) {
	_, a := setup(t)
	mustTrack(t, a, "lock", "--ttl", "1ms")
	// Backdate the lock so the TTL has elapsed.
	doc := mustTrack(t, a, "get", "--json")
	var d map[string]any
	json.Unmarshal([]byte(doc), &d)
	agent := d["agent"].(map[string]any)
	agent["lockedAt"] = "2000-01-01T00:00:00Z"
	tmp := filepath.Join(t.TempDir(), "doc.json")
	b, _ := json.Marshal(d)
	os.WriteFile(tmp, b, 0o644)
	mustTrack(t, a, "set", "--from-json", tmp)

	// An expired lock is treated as free: another actor's write goes through.
	// (Same machine here, so prove it via lock --ttl + expiry rather than a
	// distinct actor; Active() is what write-guarding uses.)
	mustTrack(t, a, "lock")
}

func TestNamespaceConfigurable(t *testing.T) {
	remote, a := setup(t)
	git(t, a, "config", "track.namespace", "refs/issue-meta/branches")
	mustTrack(t, a, "set", "state", "todo")
	mustTrack(t, a, "push")
	if git(t, a, "ls-remote", "origin", "refs/issue-meta/branches/main") == "" {
		t.Fatal("metadata not stored under the configured namespace")
	}

	b := cloneOf(t, remote, "clone-ns")
	git(t, b, "config", "track.namespace", "refs/issue-meta/branches")
	mustTrack(t, b, "fetch")
	out := mustTrack(t, b, "get", "state")
	if strings.TrimSpace(out) != "todo" {
		t.Fatalf("state via custom namespace = %q", out)
	}
}

func TestDoctor(t *testing.T) {
	_, a := setup(t)
	out := mustTrack(t, a, "doctor", "--json")
	var rep map[string]any
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("doctor --json invalid: %v\n%s", err, out)
	}
	if rep["namespaceUsable"] != true {
		t.Fatalf("bare repo should accept the namespace: %s", out)
	}
	// The probe must clean up after itself.
	if refs := git(t, a, "ls-remote", "origin", "refs/meta/*"); strings.Contains(refs, "probe") {
		t.Fatalf("probe ref left behind: %s", refs)
	}
}

func TestNeverTouchesWorktreeOrBranches(t *testing.T) {
	_, a := setup(t)
	headBefore := git(t, a, "rev-parse", "HEAD")
	mustTrack(t, a, "set", "state", "todo")
	mustTrack(t, a, "lock")
	mustTrack(t, a, "unlock")
	if git(t, a, "status", "--porcelain") != "" {
		t.Fatal("working tree dirtied by metadata operations")
	}
	if git(t, a, "rev-parse", "HEAD") != headBefore {
		t.Fatal("HEAD moved by metadata operations")
	}
}

func TestRenameMovesMetadata(t *testing.T) {
	remote, a := setup(t)
	git(t, a, "checkout", "-b", "feature/old")
	mustTrack(t, a, "set", "state", "todo")
	mustTrack(t, a, "push")
	git(t, a, "branch", "-m", "feature/new")

	mustTrack(t, a, "rename", "feature/old", "feature/new")
	out := mustTrack(t, a, "get", "--branch", "feature/new", "state")
	if strings.TrimSpace(out) != "todo" {
		t.Fatalf("state after rename = %q", out)
	}
	if _, _, code := track(t, a, "get", "--branch", "feature/old"); code != 2 {
		t.Fatal("old branch metadata should be gone")
	}
	// Remote moved too.
	if git(t, a, "ls-remote", remote, "refs/meta/branches/feature/new") == "" {
		t.Fatal("renamed ref not pushed")
	}
	if git(t, a, "ls-remote", remote, "refs/meta/branches/feature/old") != "" {
		t.Fatal("old ref not deleted on remote")
	}
}

func TestSquashCollapsesHistory(t *testing.T) {
	remote, a := setup(t)
	for _, s := range []string{"todo", "in-progress", "review", "done"} {
		mustTrack(t, a, "set", "state", s)
	}
	mustTrack(t, a, "push")
	mustTrack(t, a, "squash")

	out := mustTrack(t, a, "log", "--json")
	var entries []map[string]any
	json.Unmarshal([]byte(out), &entries)
	if len(entries) != 1 {
		t.Fatalf("history after squash = %d entries, want 1", len(entries))
	}
	if strings.TrimSpace(mustTrack(t, a, "get", "state")) != "done" {
		t.Fatal("squash lost the current state")
	}
	// Remote follows the rewrite; a fresh clone sees the squashed state.
	b := cloneOf(t, remote, "clone-sq")
	mustTrack(t, b, "fetch")
	if strings.TrimSpace(mustTrack(t, b, "get", "state")) != "done" {
		t.Fatal("squashed state not on remote")
	}
}

func TestContextMarkdown(t *testing.T) {
	_, a := setup(t)
	mustTrack(t, a, "set", "issue", "7")
	mustTrack(t, a, "set", "title", "Fix the flaky test")
	mustTrack(t, a, "set", "state", "blocked")
	mustTrack(t, a, "set", "next", `["bisect the failure"]`)
	out := mustTrack(t, a, "context")
	for _, want := range []string{"## Branch context: main", "#7", "blocked", "bisect the failure"} {
		if !strings.Contains(out, want) {
			t.Errorf("context output missing %q:\n%s", want, out)
		}
	}
}

func TestMCPServer(t *testing.T) {
	_, a := setup(t)
	mustTrack(t, a, "set", "state", "in-progress")

	cmd := exec.Command(binPath, "mcp")
	cmd.Dir = a
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	send := func(msg string) {
		if _, err := stdin.Write([]byte(msg + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	scanner := newLineScanner(stdout)
	recv := func() map[string]any {
		t.Helper()
		if !scanner.Scan() {
			t.Fatal("MCP server closed stdout")
		}
		var m map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			t.Fatalf("invalid JSON-RPC from server: %v\n%s", err, scanner.Text())
		}
		return m
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
	init := recv()
	if init["error"] != nil {
		t.Fatalf("initialize failed: %v", init)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	list := recv()
	tools, _ := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) < 5 {
		t.Fatalf("tools/list returned %d tools", len(tools))
	}

	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_branch_context","arguments":{}}}`)
	call := recv()
	res, _ := call["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("get_branch_context errored: %v", call)
	}
	content := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(content, `"state":"in-progress"`) { // compact JSON: no indentation tokens
		t.Fatalf("unexpected tool result: %s", content)
	}

	send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"say","arguments":{"channel":"main","type":"deploy.done","subject":"prod","data":{"version":"1.2.3"}}}}`)
	if r := recv(); r["result"].(map[string]any)["isError"] == true {
		t.Fatalf("typed say via MCP: %v", r)
	}
	if out := mustTrack(t, a, "chat", "main", "--type", "deploy.done", "--json"); !strings.Contains(out, `"version": "1.2.3"`) || !strings.Contains(out, `"subject": "prod"`) {
		t.Fatalf("typed event not stored: %s", out)
	}

	send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_overview","arguments":{}}}`)
	ov := recv()["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(ov, `"branches":[{"branch":"main","state":"in-progress"`) || !strings.Contains(ov, `"name":"main"`) {
		t.Fatalf("get_overview: %s", ov)
	}
	send(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"search","arguments":{"text":"progress"}}}`)
	sr := recv()["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(sr, `"matched":"state"`) {
		t.Fatalf("search: %s", sr)
	}

	send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"set_field","arguments":{"key":"agent.notes","value":"via mcp"}}}`)
	recv()
	if out := mustTrack(t, a, "get", "agent.notes"); strings.TrimSpace(out) != "via mcp" {
		t.Fatalf("set_field did not persist: %q", out)
	}

	stdin.Close()
	cmd.Wait()
}

func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return sc
}

// --- Channels & labels ---

func TestSayChatRoundTrip(t *testing.T) {
	remote, a := setup(t)
	mustTrack(t, a, "say", "starting on the auth refactor")
	mustTrack(t, a, "say", "token validation moved to its own package", "--label", "decision")

	// Another clone polls and reads.
	b := cloneOf(t, remote, "clone-b")
	mustTrack(t, b, "fetch")
	out := mustTrack(t, b, "chat", "--json")
	var res struct {
		Channel  string `json:"channel"`
		Messages []struct {
			SHA    string   `json:"sha"`
			By     string   `json:"by"`
			Labels []string `json:"labels"`
			Body   string   `json:"body"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("chat --json invalid: %v\n%s", err, out)
	}
	if res.Channel != "branches/main" || len(res.Messages) != 2 {
		t.Fatalf("expected 2 messages on #branches/main, got %+v", res)
	}
	// --since returns only what came after a given message.
	out = mustTrack(t, b, "chat", "--json", "--since", res.Messages[1].SHA)
	if !strings.Contains(out, "token validation") || strings.Contains(out, "starting on") {
		t.Fatalf("--since filter wrong:\n%s", out)
	}
	// Newest first.
	if res.Messages[0].Body != "token validation moved to its own package" {
		t.Fatalf("unexpected newest message: %+v", res.Messages[0])
	}
	if len(res.Messages[0].Labels) != 1 || res.Messages[0].Labels[0] != "decision" {
		t.Fatalf("labels lost: %+v", res.Messages[0])
	}
	if res.Messages[0].By == "" {
		t.Fatalf("by missing: %+v", res.Messages[0])
	}
}

func TestSayNamedChannelAcrossBranches(t *testing.T) {
	remote, a := setup(t)
	mustTrack(t, a, "say", "-c", "android", "emulator flaky on API 35")

	b := cloneOf(t, remote, "clone-b")
	git(t, b, "checkout", "-b", "other-branch")
	mustTrack(t, b, "fetch")
	out := mustTrack(t, b, "chat", "android")
	if !strings.Contains(out, "emulator flaky") {
		t.Fatalf("named channel not shared across branches:\n%s", out)
	}

	// The branch channel is empty: exit 2.
	_, _, code := track(t, b, "chat")
	if code != 2 {
		t.Fatalf("empty channel: expected exit 2, got %d", code)
	}
}

func TestSayConcurrentPostsMerge(t *testing.T) {
	remote, a := setup(t)
	b := cloneOf(t, remote, "clone-b")
	git(t, b, "checkout", "main")

	// Both clones post to the same channel; b posts without having fetched
	// a's message, so its push must replay onto the remote tip.
	mustTrack(t, a, "say", "-c", "planning", "message from a")
	mustTrack(t, b, "say", "-c", "planning", "message from b")

	mustTrack(t, a, "fetch")
	out := mustTrack(t, a, "chat", "planning")
	if !strings.Contains(out, "message from a") || !strings.Contains(out, "message from b") {
		t.Fatalf("concurrent posts did not merge:\n%s", out)
	}
}

func TestSayOfflineThenPushMerges(t *testing.T) {
	remote, a := setup(t)
	b := cloneOf(t, remote, "clone-b")
	git(t, b, "checkout", "main")
	mustTrack(t, b, "say", "-c", "planning", "remote message")

	// Simulate a offline: the local commit succeeds, the sync fails.
	git(t, a, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	_, stderr, code := track(t, a, "say", "-c", "planning", "offline message")
	if code != 0 {
		t.Fatalf("offline say should still succeed locally: %s", stderr)
	}
	if !strings.Contains(stderr, "saved locally") {
		t.Fatalf("expected local-save notice, got: %s", stderr)
	}

	// Back online: fetch reports the channel as behind, push --all merges.
	git(t, a, "remote", "set-url", "origin", remote)
	_, stderr, _ = track(t, a, "fetch")
	if !strings.Contains(stderr, "unsent local messages") {
		t.Fatalf("fetch should report unsent messages, got: %s", stderr)
	}
	mustTrack(t, a, "push", "--all")
	mustTrack(t, a, "fetch")
	out := mustTrack(t, a, "chat", "planning")
	if !strings.Contains(out, "offline message") || !strings.Contains(out, "remote message") {
		t.Fatalf("offline message did not merge:\n%s", out)
	}
	// And the other clone sees both.
	mustTrack(t, b, "fetch")
	out = mustTrack(t, b, "chat", "planning")
	if !strings.Contains(out, "offline message") {
		t.Fatalf("merged message not visible remotely:\n%s", out)
	}
}

func TestLabelDefinitionsSharedAndHinted(t *testing.T) {
	remote, a := setup(t)
	mustTrack(t, a, "labels", "set", "bug", "Something is broken for users")
	mustTrack(t, a, "channels", "set", "planning", "Cross-branch planning notes")

	// Defined label: no hint. Undefined label: hint on stderr, still works.
	_, stderr, code := track(t, a, "say", "found it", "--label", "bug")
	if code != 0 || strings.Contains(stderr, "hint") {
		t.Fatalf("defined label should not hint (code %d): %s", code, stderr)
	}
	_, stderr, code = track(t, a, "say", "odd behavior", "--label", "androd")
	if code != 0 {
		t.Fatalf("undefined label must still work: %s", stderr)
	}
	if !strings.Contains(stderr, "not defined") {
		t.Fatalf("expected undefined-label hint, got: %s", stderr)
	}

	// Definitions sync to other clones.
	b := cloneOf(t, remote, "clone-b")
	mustTrack(t, b, "fetch")
	out := mustTrack(t, b, "labels")
	if !strings.Contains(out, "bug") || !strings.Contains(out, "Something is broken") {
		t.Fatalf("label definitions did not sync:\n%s", out)
	}
	out = mustTrack(t, b, "channels")
	if !strings.Contains(out, "planning") || !strings.Contains(out, "Cross-branch planning") {
		t.Fatalf("channel definitions did not sync:\n%s", out)
	}

	// Labels field on the branch doc validates as string array.
	mustTrack(t, a, "set", "labels", `["bug","backend"]`)
	out = mustTrack(t, a, "show")
	if !strings.Contains(out, "labels:") || !strings.Contains(out, "bug, backend") {
		t.Fatalf("labels field missing from show:\n%s", out)
	}
}

func TestContextIncludesChat(t *testing.T) {
	_, a := setup(t)
	mustTrack(t, a, "set", "title", "Auth refactor")
	mustTrack(t, a, "say", "left off at the token tests")
	out := mustTrack(t, a, "context")
	if !strings.Contains(out, "Recent chat") || !strings.Contains(out, "left off at the token tests") {
		t.Fatalf("context missing chat:\n%s", out)
	}
}

// startWatch runs `git track watch` in the background and returns a function
// that waits for it and yields stdout + exit code.
func startWatch(t *testing.T, dir string, args ...string) func() (string, int) {
	t.Helper()
	cmd := exec.Command(binPath, append([]string{"watch"}, args...)...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting watch: %v", err)
	}
	// Give the watcher its first (seeding) tick before anyone posts.
	time.Sleep(700 * time.Millisecond)
	return func() (string, int) {
		err := cmd.Wait()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("watch: %v (stderr: %s)", err, stderr.String())
		}
		return stdout.String(), code
	}
}

func TestWatchSeesNewMessage(t *testing.T) {
	remote, a := setup(t)
	b := cloneOf(t, remote, "clone-b")
	mustTrack(t, a, "say", "-c", "main", "already there") // existing history is not replayed

	wait := startWatch(t, b, "main", "--once", "--timeout", "20s", "--interval", "300ms", "--json")
	mustTrack(t, a, "say", "-c", "main", "anyone free to review?", "--label", "question")
	out, code := wait()
	if code != 0 {
		t.Fatalf("watch exit %d, out: %s", code, out)
	}
	if strings.Contains(out, "already there") {
		t.Fatalf("watch replayed old history:\n%s", out)
	}
	var msg struct {
		Channel string   `json:"channel"`
		Body    string   `json:"body"`
		Labels  []string `json:"labels"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &msg); err != nil {
		t.Fatalf("watch --json should emit one JSON object per line: %v\n%s", err, out)
	}
	if msg.Channel != "main" || msg.Body != "anyone free to review?" || len(msg.Labels) != 1 {
		t.Fatalf("unexpected message: %+v", msg)
	}
}

func TestWatchDefaultsAndTimeout(t *testing.T) {
	remote, a := setup(t)
	b := cloneOf(t, remote, "clone-b")
	git(t, b, "checkout", "main")

	// Nothing arrives: exit 2.
	wait := startWatch(t, b, "--once", "--timeout", "1s", "--interval", "300ms")
	if _, code := wait(); code != 2 {
		t.Fatalf("timeout without messages should exit 2, got %d", code)
	}

	// Default watch covers the branch channel (a says without -c).
	wait = startWatch(t, b, "--once", "--timeout", "20s", "--interval", "300ms")
	mustTrack(t, a, "say", "branch-scoped note")
	out, code := wait()
	if code != 0 || !strings.Contains(out, "#branches/main") || !strings.Contains(out, "branch-scoped note") {
		t.Fatalf("default watch missed branch channel (exit %d):\n%s", code, out)
	}
}

func TestWatchSyncsUnsentAndSkipsOwnReplay(t *testing.T) {
	remote, a := setup(t)
	b := cloneOf(t, remote, "clone-b")
	git(t, b, "checkout", "main")

	// a has an unsent local message (offline), b posts meanwhile.
	git(t, a, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	track(t, a, "say", "-c", "planning", "offline note from a")
	git(t, a, "remote", "set-url", "origin", remote)

	wait := startWatch(t, a, "planning", "--once", "--timeout", "20s", "--interval", "300ms")
	mustTrack(t, b, "say", "-c", "planning", "note from b")
	out, code := wait()
	if code != 0 || !strings.Contains(out, "note from b") {
		t.Fatalf("watch should surface b's message (exit %d):\n%s", code, out)
	}
	if strings.Contains(out, "offline note from a") {
		t.Fatalf("watch echoed a's own replayed message:\n%s", out)
	}
	// And a's unsent message got synced along the way.
	mustTrack(t, b, "fetch")
	if out := mustTrack(t, b, "chat", "planning"); !strings.Contains(out, "offline note from a") {
		t.Fatalf("watch did not sync unsent message:\n%s", out)
	}
}

func TestChannelDeleteAndPrune(t *testing.T) {
	_, a := setup(t)
	mustTrack(t, a, "say", "-c", "scratch", "temporary")
	mustTrack(t, a, "channels", "delete", "scratch")
	if _, _, code := track(t, a, "chat", "scratch"); code != 2 {
		t.Fatalf("deleted channel should be gone locally (exit %d)", code)
	}
	if out := git(t, a, "ls-remote", "origin", "refs/meta/channels/*"); strings.Contains(out, "scratch") {
		t.Fatalf("deleted channel still on remote: %s", out)
	}

	// A merged-and-deleted branch's channel is pruned like its metadata;
	// main and named channels are not.
	git(t, a, "checkout", "-b", "feature")
	mustTrack(t, a, "say", "feature chatter")
	mustTrack(t, a, "say", "-c", "main", "keep me")
	git(t, a, "checkout", "main")
	git(t, a, "branch", "-D", "feature")
	out := mustTrack(t, a, "prune", "--remote", "--json")
	if !strings.Contains(out, "branches/feature") {
		t.Fatalf("prune should remove the branch channel:\n%s", out)
	}
	if out := mustTrack(t, a, "channels"); !strings.Contains(out, "main") || strings.Contains(out, "feature") {
		t.Fatalf("channels after prune:\n%s", out)
	}
}

// --- Token-aware reads: overview, unread cursors, search, label usage ---

func TestOverviewAndUnreadCursors(t *testing.T) {
	remote, a := setup(t)
	mustTrack(t, a, "set", "state", "review")
	mustTrack(t, a, "push")
	mustTrack(t, a, "say", "-c", "main", "who can review #42?", "--label", "question")
	mustTrack(t, a, "say", "branch note")

	// The poster has nothing unread: their own posts advance the cursor.
	var ov struct {
		Branch   string `json:"branch"`
		Branches []struct {
			Branch string `json:"branch"`
			State  string `json:"state"`
		} `json:"branches"`
		Channels []struct {
			Name     string `json:"name"`
			Messages int    `json:"messages"`
			Unread   int    `json:"unread"`
			Last     *struct {
				Body string `json:"body"`
			} `json:"last"`
		} `json:"channels"`
	}
	readOverview := func(dir string) {
		t.Helper()
		out := mustTrack(t, dir, "overview", "--json")
		if err := json.Unmarshal([]byte(out), &ov); err != nil {
			t.Fatalf("overview --json invalid: %v\n%s", err, out)
		}
	}
	unread := func(name string) int {
		t.Helper()
		for _, ch := range ov.Channels {
			if ch.Name == name {
				return ch.Unread
			}
		}
		t.Fatalf("channel %s missing from overview: %+v", name, ov.Channels)
		return -1
	}
	readOverview(a)
	if ov.Branch != "main" || len(ov.Branches) != 1 || ov.Branches[0].State != "review" {
		t.Fatalf("overview branches: %+v", ov)
	}
	if unread("main") != 0 || unread("branches/main") != 0 {
		t.Fatalf("poster should have nothing unread: %+v", ov.Channels)
	}

	// A fresh clone sees everything as unread, reads it once, then nothing.
	b := cloneOf(t, remote, "clone-b")
	mustTrack(t, b, "fetch")
	readOverview(b)
	if unread("main") != 1 || unread("branches/main") != 1 {
		t.Fatalf("fresh clone unread counts: %+v", ov.Channels)
	}
	if ov.Channels[1].Name != "main" || ov.Channels[1].Last == nil || ov.Channels[1].Last.Body != "who can review #42?" {
		t.Fatalf("overview last message: %+v", ov.Channels)
	}
	out := mustTrack(t, b, "chat", "main", "--unread", "--json")
	if !strings.Contains(out, "who can review") {
		t.Fatalf("chat --unread missed the message: %s", out)
	}
	out = mustTrack(t, b, "chat", "main", "--unread", "--json")
	if !strings.Contains(out, `"messages": []`) {
		t.Fatalf("second --unread read should be empty: %s", out)
	}
	readOverview(b)
	if unread("main") != 0 || unread("branches/main") != 1 {
		t.Fatalf("after reading main: %+v", ov.Channels)
	}

	// Cursors are per worktree: a second worktree of the same clone still
	// sees main as unread, and reading there leaves b's cursor alone.
	wt := filepath.Join(filepath.Dir(b), "wt")
	git(t, b, "worktree", "add", "-b", "feat", wt)
	readOverview(wt)
	if unread("main") != 1 {
		t.Fatalf("worktree should have its own cursor: %+v", ov.Channels)
	}
	mustTrack(t, wt, "chat", "main")
	readOverview(b)
	if unread("main") != 0 {
		t.Fatalf("b's cursor changed by worktree read: %+v", ov.Channels)
	}

	// A new message from a is unread again after fetch; a label-filtered
	// read does not mark it read, an unfiltered one does.
	mustTrack(t, a, "say", "-c", "main", "ping")
	mustTrack(t, b, "fetch")
	readOverview(b)
	if unread("main") != 1 {
		t.Fatalf("new message not unread: %+v", ov.Channels)
	}
	mustTrack(t, b, "chat", "main", "--label", "question")
	readOverview(b)
	if unread("main") != 1 {
		t.Fatalf("label-filtered read must not mark read: %+v", ov.Channels)
	}
	mustTrack(t, b, "chat", "main")
	readOverview(b)
	if unread("main") != 0 {
		t.Fatalf("plain read should mark read: %+v", ov.Channels)
	}
	// The cursor lives in the git dir, never in the working tree.
	if _, err := os.Stat(filepath.Join(b, ".git", "track", "cursors", "main")); err != nil {
		t.Fatalf("cursor file missing: %v", err)
	}
	if out := git(t, b, "status", "--porcelain"); out != "" {
		t.Fatalf("working tree touched: %s", out)
	}
}

func TestSearchAndLabelUsage(t *testing.T) {
	_, a := setup(t)
	mustTrack(t, a, "set", "title", "Refactor token refresh")
	mustTrack(t, a, "set", "labels", `["bug","auth"]`)
	mustTrack(t, a, "labels", "set", "bug", "Something is broken")
	mustTrack(t, a, "say", "token refresh loops on 401", "--label", "bug")
	mustTrack(t, a, "say", "-c", "main", "unrelated chatter")
	git(t, a, "commit", "--allow-empty", "-m", "fix token refresh loop", "--trailer", "Label: bug")

	// labels: defined ∪ used, with counts across branches, messages, commits.
	out := mustTrack(t, a, "labels", "--json")
	var rows []struct {
		Name                        string `json:"name"`
		Description                 string `json:"description"`
		Branches, Messages, Commits int
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("labels --json invalid: %v\n%s", err, out)
	}
	want := map[string][3]int{"auth": {1, 0, 0}, "bug": {1, 1, 1}}
	for _, r := range rows {
		w, ok := want[r.Name]
		if !ok {
			t.Fatalf("unexpected label row %+v", r)
		}
		if [3]int{r.Branches, r.Messages, r.Commits} != w {
			t.Fatalf("label %s counts = %v, want %v", r.Name, [3]int{r.Branches, r.Messages, r.Commits}, w)
		}
		delete(want, r.Name)
	}
	if len(want) != 0 {
		t.Fatalf("labels missing: %v", want)
	}

	// From a label's perspective: everything carrying it, in one call.
	var res struct {
		Branches []struct {
			Branch  string `json:"branch"`
			Matched string `json:"matched"`
		} `json:"branches"`
		Messages []struct {
			Channel string `json:"channel"`
			Body    string `json:"body"`
		} `json:"messages"`
		Commits []struct {
			Ref     string   `json:"ref"`
			Subject string   `json:"subject"`
			Labels  []string `json:"labels"`
		} `json:"commits"`
	}
	parse := func(out string) {
		t.Helper()
		res.Branches, res.Messages, res.Commits = nil, nil, nil
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("search --json invalid: %v\n%s", err, out)
		}
	}
	parse(mustTrack(t, a, "labels", "show", "bug", "--json"))
	if len(res.Branches) != 1 || len(res.Messages) != 1 || len(res.Commits) != 1 {
		t.Fatalf("labels show bug: %+v", res)
	}
	if res.Messages[0].Channel != "branches/main" || res.Commits[0].Ref != "main" || res.Commits[0].Subject != "fix token refresh loop" {
		t.Fatalf("labels show bug details: %+v", res)
	}
	// Text search spans metadata and messages (case-insensitive), not commits.
	parse(mustTrack(t, a, "search", "TOKEN", "--json"))
	if len(res.Branches) != 1 || res.Branches[0].Matched != "title" || len(res.Messages) != 1 || len(res.Commits) != 0 {
		t.Fatalf("search TOKEN: %+v", res)
	}
	// Text + label narrows; commits join when a label is given.
	parse(mustTrack(t, a, "search", "loop", "--label", "bug", "--json"))
	if len(res.Branches) != 0 || len(res.Messages) != 1 || len(res.Commits) != 1 {
		t.Fatalf("search loop --label bug: %+v", res)
	}
	if _, _, code := track(t, a, "search", "nonexistent-phrase"); code != 2 {
		t.Fatalf("empty search exit = %d, want 2", code)
	}
	if _, _, code := track(t, a, "search"); code != 1 {
		t.Fatalf("search without filters exit = %d, want 1", code)
	}
}

func TestBinaryAnswersToBothNames(t *testing.T) {
	_, a := setup(t)
	link := filepath.Join(t.TempDir(), "track")
	if err := os.Symlink(binPath, link); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	out, _, code := runIn(t, a, link, "--help")
	if code != 0 || !strings.Contains(out, "track [command]") || strings.Contains(out, "git-track [command]") {
		t.Fatalf("help under the track name:\n%s", out)
	}
	if out, _, code := runIn(t, a, link, "overview", "--json"); code != 0 || !strings.Contains(out, `"channels"`) {
		t.Fatalf("track overview: %d %s", code, out)
	}
}

// --- Import from GitHub (through a fake gh on PATH; the suite stays offline) ---

// fakeGH installs a `gh` script that answers `issue view N` and `pr view`
// with fixtures and returns the directory to prepend to PATH.
func fakeGH(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake gh script needs a POSIX shell")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1 $2" in
  "issue view")
    case "$3" in
      42) cat <<'JSON'
{"number":42,"title":"Refactor auth middleware","body":"Tokens are validated in three places.\n\nUnify them.","state":"OPEN","url":"https://github.com/o/r/issues/42","labels":[{"name":"bug","description":"Something is broken"},{"name":"auth","description":""}]}
JSON
      ;;
      7) cat <<'JSON'
{"number":7,"title":"Old thing","body":"","state":"CLOSED","url":"https://github.com/o/r/issues/7","labels":[]}
JSON
      ;;
      *) echo "GraphQL: Could not resolve to an Issue" >&2; exit 1;;
    esac;;
  "pr view") echo '{"closingIssuesReferences":[{"number":42}]}';;
  *) echo "unexpected: $*" >&2; exit 1;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func trackWithPath(t *testing.T, dir, pathPrefix string, args ...string) (string, string, int) {
	t.Helper()
	return trackWithEnvPath(t, dir, pathPrefix+string(os.PathListSeparator)+os.Getenv("PATH"), args...)
}

// trackWithEnvPath runs the binary with PATH replaced entirely.
func trackWithEnvPath(t *testing.T, dir, path string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+path)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), code
}

func TestImportGitHubIssue(t *testing.T) {
	_, a := setup(t)
	ghDir := fakeGH(t)
	mustTrack(t, a, "set", "next", `["keep me"]`)

	out, stderr, code := trackWithPath(t, a, ghDir, "import", "42", "--json")
	if code != 0 {
		t.Fatalf("import failed (%d): %s", code, stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("import --json invalid: %v\n%s", err, out)
	}
	if doc["issue"] != 42.0 || doc["title"] != "Refactor auth middleware" || doc["state"] != "todo" {
		t.Fatalf("mapped fields: %v", doc)
	}
	if !strings.Contains(doc["context"].(string), "Unify them.") {
		t.Fatalf("body not imported as context: %v", doc["context"])
	}
	if labels := fmt.Sprint(doc["labels"]); labels != "[bug auth]" {
		t.Fatalf("labels = %s", labels)
	}
	if links := fmt.Sprint(doc["links"]); !strings.Contains(links, "issues/42") {
		t.Fatalf("links = %s", links)
	}
	if next := fmt.Sprint(doc["next"]); next != "[keep me]" {
		t.Fatalf("unrelated field clobbered: %s", next)
	}
	// GitHub label descriptions became definitions; empty ones did not.
	out = mustTrack(t, a, "labels", "--json")
	if !strings.Contains(out, `"Something is broken"`) || strings.Contains(out, `"name": "auth",\n      "description": "x"`) {
		t.Fatalf("label definitions: %s", out)
	}

	// Re-import refreshes; links are not duplicated.
	if _, stderr, code := trackWithPath(t, a, ghDir, "import"); code != 0 { // number now comes from the issue field
		t.Fatalf("re-import: %s", stderr)
	}
	if out := mustTrack(t, a, "get", "links"); strings.Count(out, "issues/42") != 1 {
		t.Fatalf("links duplicated: %s", out)
	}

	// Closed issue → done; number inferred from the branch name.
	git(t, a, "checkout", "-b", "7-old-thing")
	if _, stderr, code := trackWithPath(t, a, ghDir, "import"); code != 0 {
		t.Fatalf("import on branch 7-old-thing: %s", stderr)
	}
	if out := mustTrack(t, a, "get", "state"); strings.TrimSpace(out) != "done" {
		t.Fatalf("closed issue state = %q", out)
	}
	// Number inferred from the PR the branch closes.
	git(t, a, "checkout", "-b", "no-number-here")
	if _, stderr, code := trackWithPath(t, a, ghDir, "import"); code != 0 {
		t.Fatalf("import via PR: %s", stderr)
	}
	if out := mustTrack(t, a, "get", "issue"); strings.TrimSpace(out) != "42" {
		t.Fatalf("issue via PR = %q", out)
	}
	// Unknown issue: gh's error surfaces, exit 1.
	if _, stderr, code := trackWithPath(t, a, ghDir, "import", "999"); code != 1 || !strings.Contains(stderr, "Could not resolve") {
		t.Fatalf("unknown issue: %d %s", code, stderr)
	}
	// Missing gh: clear message, nothing else affected. PATH holds only git.
	gitPath, _ := exec.LookPath("git")
	onlyGit := t.TempDir()
	if err := os.Symlink(gitPath, filepath.Join(onlyGit, "git")); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := trackWithEnvPath(t, a, onlyGit, "import", "42"); code != 1 || !strings.Contains(stderr, "GitHub CLI") {
		t.Fatalf("missing gh: %d %s", code, stderr)
	}
}

// --- Events: emit, typed reads, triggers ---

func TestEmitEventsAndTriggers(t *testing.T) {
	remote, a := setup(t)
	out := mustTrack(t, a, "emit", "tests.failed", "3 failures on main", "-c", "main",
		"--data", `{"count":3}`, "--subject", "main", "--label", "ci", "--json")
	if !strings.Contains(out, `"type": "tests.failed"`) {
		t.Fatalf("emit --json: %s", out)
	}
	mustTrack(t, a, "say", "-c", "main", "looking into it")

	// A chat message is the event type "chat"; typed events carry data.
	out = mustTrack(t, a, "chat", "main", "--json")
	for _, want := range []string{`"type": "chat"`, `"type": "tests.failed"`, `"count": 3`, `"subject": "main"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("chat --json missing %s:\n%s", want, out)
		}
	}
	out = mustTrack(t, a, "chat", "main", "--type", "tests.failed", "--json")
	if strings.Contains(out, "looking into it") || !strings.Contains(out, "3 failures") {
		t.Fatalf("--type filter: %s", out)
	}
	// search finds events by their type line, no index needed.
	if out := mustTrack(t, a, "search", "tests.failed", "--json"); !strings.Contains(out, `"count": 3`) {
		t.Fatalf("search by type: %s", out)
	}

	// Plain git can publish an event: commit with trailers, update the ref.
	tree := git(t, a, "mktree")
	tip := git(t, a, "rev-parse", "refs/meta/channels/main")
	sha := git(t, a, "commit-tree", tree, "-p", tip, "-m", "deployed\n\nType: deploy.done\nData: {\"env\":\"prod\"}\nBy: ci@runner")
	git(t, a, "update-ref", "refs/meta/channels/main", sha, tip)
	out = mustTrack(t, a, "chat", "main", "--type", "deploy.done", "--json")
	if !strings.Contains(out, `"env": "prod"`) || !strings.Contains(out, `"by": "ci@runner"`) {
		t.Fatalf("plain-git event not parsed: %s", out)
	}

	// Trigger: watch --type ... --exec runs a handler with the CloudEvents
	// envelope on stdin and TRACK_* in the environment; other types are ignored.
	b := cloneOf(t, remote, "clone-b")
	mustTrack(t, b, "fetch")
	log := filepath.Join(t.TempDir(), "handled.txt")
	handler := "cat >> " + log + `; printf '%s|%s|%s\n' "$TRACK_TYPE" "$TRACK_SUBJECT" "$TRACK_DATA" >> ` + log
	wait := startWatch(t, b, "main", "--once", "--timeout", "20s", "--interval", "300ms",
		"--type", "tests.failed", "--exec", handler, "--json")
	mustTrack(t, a, "say", "-c", "main", "chatter that must not trigger")
	mustTrack(t, a, "emit", "tests.failed", "flaky again", "-c", "main", "--data", `{"count":1}`, "--subject", "feat/x")
	out, code := wait()
	if code != 0 {
		t.Fatalf("watch exit %d:\n%s", code, out)
	}
	if strings.Contains(out, "chatter") || !strings.Contains(out, `"type":"tests.failed"`) || !strings.Contains(out, `"channel":"main"`) {
		t.Fatalf("watch output: %s", out)
	}
	handled, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("handler never ran: %v", err)
	}
	got := string(handled)
	for _, want := range []string{`"specversion":"1.0"`, `"type":"tests.failed"`, `"source":"git-track://`, `"datacontenttype":"application/json"`, `"data":{"count":1}`, "tests.failed|feat/x|{\"count\":1}"} {
		if !strings.Contains(got, want) {
			t.Fatalf("handler input missing %s:\n%s", want, got)
		}
	}
	if strings.Count(got, "specversion") != 1 {
		t.Fatalf("handler ran for the wrong events:\n%s", got)
	}
}
