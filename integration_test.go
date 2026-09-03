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
	"strings"
	"testing"
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
	if !strings.Contains(content, `"state": "in-progress"`) {
		t.Fatalf("unexpected tool result: %s", content)
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
