package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/robertdewilde-dev/git-track/internal/schema"
	"github.com/robertdewilde-dev/git-track/internal/store"
	"github.com/spf13/cobra"
)

// mcpCmd runs a Model Context Protocol server over stdio so agents can read
// and write branch metadata natively instead of shelling out and parsing
// --json. The protocol is JSON-RPC 2.0, one message per line.
var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Run an MCP (Model Context Protocol) stdio server exposing branch metadata",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newCtx()
		if err != nil {
			return err
		}
		return serveMCP(c)
	},
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func serveMCP(c *appCtx) error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	out := json.NewEncoder(os.Stdout)

	reply := func(id json.RawMessage, result any, rerr *rpcError) {
		if id == nil {
			return // notification: no response
		}
		msg := map[string]any{"jsonrpc": "2.0", "id": id}
		if rerr != nil {
			msg["error"] = rerr
		} else {
			msg["result"] = result
		}
		_ = out.Encode(msg)
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			reply(req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "git-track", "version": "1.0.0"},
			}, nil)
		case "notifications/initialized", "notifications/cancelled":
			// no-op
		case "ping":
			reply(req.ID, map[string]any{}, nil)
		case "tools/list":
			reply(req.ID, map[string]any{"tools": mcpTools()}, nil)
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				reply(req.ID, nil, &rpcError{Code: -32602, Message: "invalid params"})
				continue
			}
			text, isErr := callMCPTool(c, params.Name, params.Arguments)
			reply(req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
				"isError": isErr,
			}, nil)
		default:
			reply(req.ID, nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method})
		}
	}
	return scanner.Err()
}

// mcpTools lists the tools. Descriptions are deliberately terse: every word
// here is loaded into the agent's context on every session.
func mcpTools() []map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	num := func(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }
	boolean := func(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
	strs := func(desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
	}
	branch := str("Branch name (default: checked-out branch)")
	channel := str("Channel: 'main' (shared coordination), a topic name, or 'branches/<branch>' (default: this branch's channel)")
	tool := func(name, desc string, props map[string]any, required ...string) map[string]any {
		if required == nil {
			required = []string{}
		}
		return map[string]any{
			"name":        name,
			"description": desc,
			"inputSchema": map[string]any{"type": "object", "properties": props, "required": required},
		}
	}
	return []map[string]any{
		tool("get_overview",
			"Start here: compact digest of all branches with metadata, all channels with unread counts and last message, and defined labels.",
			map[string]any{}),
		tool("get_branch_context",
			"Full metadata document of a branch (issue, title, state, context, next, labels, links, lock) as JSON.",
			map[string]any{"branch": branch}),
		tool("get_context_markdown",
			"Branch metadata plus recent chat as prompt-ready markdown.",
			map[string]any{"branch": branch}),
		tool("list_branches",
			"Every branch with metadata and its full document.",
			map[string]any{}),
		tool("set_field",
			"Set one metadata field; dot paths reach nested fields (agent.notes). JSON arrays/numbers in value are parsed. state must be one of the configured states (default todo, in-progress, blocked, review, done).",
			map[string]any{
				"branch": branch,
				"key":    str("Field or dot path: state, title, context, next, labels, agent.notes ..."),
				"value":  str("Value; JSON arrays/numbers are parsed"),
			}, "key", "value"),
		tool("unset_field",
			"Remove one metadata field.",
			map[string]any{"branch": branch, "key": str("Field or dot path")}, "key"),
		tool("acquire_lock",
			"Take the branch's agent lock (compare-and-swap push) before changes others might race on. Fails if another actor holds it. Set ttl so a crashed agent never wedges the branch.",
			map[string]any{
				"branch": branch,
				"ttl":    str("Auto-expiry like 30m or 2h (recommended)"),
				"force":  boolean("Steal a lock held by someone else"),
			}),
		tool("release_lock",
			"Release the branch's agent lock.",
			map[string]any{"branch": branch}),
		tool("say",
			"Post to a channel for other agents: a chat message, or with type a typed event (tests.failed, deploy.done) with optional JSON data. Syncs at once; concurrent posts merge. Then wait_for_message for a reply.",
			map[string]any{
				"text":    str("Message body"),
				"channel": channel,
				"labels":  strs("Labels like bug, decision, question (see list_labels)"),
				"type":    str("Event type, dotted lowercase (default chat)"),
				"subject": str("What the event is about (branch, file, issue)"),
				"data":    map[string]any{"type": "object", "description": "JSON payload for typed events"},
			}),
		tool("read_chat",
			"Read a channel's messages, newest first, and mark it read. unread=true returns only what arrived since you last read it here.",
			map[string]any{
				"channel": channel,
				"limit":   num("Max messages (default 20)"),
				"since":   str("Only messages after this sha"),
				"unread":  boolean("Only messages since the channel was last read here"),
				"type":    str("Only events of this type"),
			}),
		tool("wait_for_message",
			"Block until a new message lands on the watched channels or the timeout passes; returns the new messages (your own posts excluded). Default: this branch's channel and main.",
			map[string]any{
				"channels":        strs("Channels to watch"),
				"all":             boolean("Watch every channel"),
				"types":           strs("Only events of these types"),
				"timeout_seconds": num("Give up after this long (default 60; keep under your tool-call timeout)"),
			}),
		tool("search",
			"Find text (case-insensitive) and/or a label across branch metadata, chat messages in every channel, and git commits with a 'Label: <name>' trailer. Cheaper than reading channels.",
			map[string]any{
				"text":  str("Substring to look for"),
				"label": str("Only items carrying this label"),
				"limit": num("Max messages/commits (default 50)"),
			}),
		tool("import_issue",
			"Pull a GitHub issue (number, title, body, labels, state, url) into the branch's metadata via the gh CLI. Omit issue to infer it from the branch's issue field, branch name, or its PR.",
			map[string]any{"branch": branch, "issue": num("Issue number")}),
		tool("list_channels",
			"Channels with descriptions: main, branches/<branch>, and named topics.",
			map[string]any{}),
		tool("list_labels",
			"Label vocabulary with meanings and usage counts (branches, messages, commits).",
			map[string]any{}),
		tool("define_label",
			"Define or redefine a label's meaning for everyone. Labels are optional; undefined ones still work.",
			map[string]any{"name": str("Label name"), "description": str("What it means")}, "name", "description"),
		tool("define_channel",
			"Define or redefine a named channel's purpose for everyone.",
			map[string]any{"name": str("Channel name"), "description": str("What it is for")}, "name", "description"),
	}
}

// mcpPosted remembers messages posted through this server (channel + body) so
// wait_for_message does not hand an agent its own words back.
var mcpPosted = map[string]bool{}

// compact renders a tool result as single-line JSON: nobody pretty-prints
// for a model, and indentation is tokens.
func compact(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// callMCPTool executes one tool. Errors are returned as text with isError so
// the agent can read the reason (e.g. "lock held by ...").
func callMCPTool(c *appCtx, name string, args map[string]any) (string, bool) {
	str := func(key string) string {
		v, _ := args[key].(string)
		return v
	}
	strs := func(key string) []string {
		var out []string
		if arr, ok := args[key].([]any); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					out = append(out, s)
				}
			}
		}
		return out
	}
	num := func(key string, def int) int {
		if n, ok := args[key].(float64); ok && n > 0 {
			return int(n)
		}
		return def
	}
	noBranch := map[string]bool{"list_branches": true, "get_overview": true, "search": true, "list_channels": true, "list_labels": true, "define_label": true, "define_channel": true}
	branch := str("branch")
	if branch == "" {
		var err error
		if branch, err = c.branch(); err != nil && !noBranch[name] {
			return err.Error(), true
		}
	}
	fail := func(err error) (string, bool) {
		return fmt.Sprintf("%s (exit code %d)", err.Error(), exitCodeFor(err)), true
	}
	switch name {
	case "get_overview":
		ov, err := buildOverview(c)
		if err != nil {
			return fail(err)
		}
		return compact(ov), false
	case "get_branch_context":
		doc, _, err := c.store.Read(branch)
		if err != nil {
			return fail(err)
		}
		return compact(doc), false
	case "get_context_markdown":
		doc, _, err := c.store.Read(branch)
		if err != nil {
			return fail(err)
		}
		msgs, _ := c.store.Messages(store.BranchChannel(branch), 5)
		return contextMarkdown(branch, doc, msgs), false
	case "list_branches":
		branches, err := c.store.Branches()
		if err != nil {
			return fail(err)
		}
		rows := []map[string]any{}
		for _, b := range branches {
			if doc, _, err := c.store.Read(b); err == nil {
				rows = append(rows, map[string]any{"branch": b, "meta": doc})
			}
		}
		return compact(rows), false
	case "set_field":
		key := str("key")
		if key == "" || key == "schemaVersion" {
			return "invalid key", true
		}
		doc, err := c.mutateDoc(branch, false, "set "+key, func(d schema.Doc) error {
			return d.Set(key, parseValue(key, str("value")))
		})
		if err != nil {
			return fail(err)
		}
		return compact(doc), false
	case "unset_field":
		key := str("key")
		if key == "" || key == "schemaVersion" {
			return "invalid key", true
		}
		_, err := c.mutateDoc(branch, false, "unset "+key, func(d schema.Doc) error {
			if !d.Unset(key) {
				return exitErr(ExitError, "no such field: %s", key)
			}
			return nil
		})
		if err != nil {
			return fail(err)
		}
		return "removed " + key, false
	case "acquire_lock":
		var ttl time.Duration
		if s := str("ttl"); s != "" {
			var err error
			if ttl, err = time.ParseDuration(s); err != nil {
				return "invalid ttl: " + s, true
			}
		}
		force, _ := args["force"].(bool)
		value, err := acquireLock(c, branch, ttl, force)
		if err != nil {
			return fail(err)
		}
		return "locked " + branch + " as " + value, false
	case "release_lock":
		if err := releaseLock(c, branch, false); err != nil {
			return fail(err)
		}
		return "unlocked " + branch, false
	case "say":
		text := str("text")
		m := store.Message{Type: str("type"), Subject: str("subject"), Labels: strs("labels"), Body: text}
		if m.Type == "" {
			m.Type = store.ChatType
		}
		if d, ok := args["data"]; ok && d != nil {
			m.Data, _ = json.Marshal(d)
		}
		if text == "" && m.Type == store.ChatType {
			return "empty message", true
		}
		channel, sha, synced, err := publish(c, str("channel"), m)
		if err != nil {
			return fail(err)
		}
		if text == "" {
			text = m.Type
		}
		mcpPosted[channel+"\x00"+text] = true
		note := ""
		if !synced {
			note = " (saved locally; remote sync failed)"
		}
		return "posted to #" + channel + " as " + sha[:12] + note, false
	case "read_chat":
		channel := str("channel")
		if channel == "" {
			channel = store.BranchChannel(branch)
		}
		since, limit := str("since"), num("limit", 20)
		if unread, _ := args["unread"].(bool); unread {
			since, limit = c.store.Cursor(channel), 0
		}
		typ := str("type")
		if typ != "" {
			limit = 0
		}
		msgs, err := c.store.MessagesSince(channel, since, limit)
		if err != nil {
			return fail(err)
		}
		if typ != "" {
			kept := []store.Message{}
			for _, m := range msgs {
				if m.Type == typ {
					kept = append(kept, m)
				}
			}
			msgs = kept
		} else {
			_ = c.store.MarkRead(channel)
		}
		if msgs == nil {
			msgs = []store.Message{}
		}
		return compact(map[string]any{"channel": channel, "messages": msgs}), false
	case "wait_for_message":
		opts := watchOptions{interval: 2 * time.Second, timeout: 60 * time.Second, once: true}
		if n, ok := args["timeout_seconds"].(float64); ok && n > 0 {
			opts.timeout = time.Duration(n * float64(time.Second))
		}
		opts.all, _ = args["all"].(bool)
		opts.channels = strs("channels")
		opts.types = strs("types")
		if len(opts.channels) == 0 && !opts.all {
			opts.channels = []string{store.MainChannel, store.BranchChannel(branch)}
		}
		got := []store.ChannelMessage{}
		_, err := watchChannels(c, opts, func(channel string, m store.Message) {
			if mcpPosted[channel+"\x00"+m.Body] {
				return // our own post echoing back
			}
			got = append(got, store.ChannelMessage{Channel: channel, Message: m})
		})
		if err != nil {
			return fail(err)
		}
		if len(got) == 0 {
			return fmt.Sprintf("no new messages within %s", opts.timeout), false
		}
		return compact(map[string]any{"messages": got}), false
	case "search":
		res, err := runSearch(c, str("text"), str("label"), num("limit", 50))
		if err != nil {
			return fail(err)
		}
		return compact(res), false
	case "import_issue":
		arg := ""
		if n := num("issue", 0); n > 0 {
			arg = strconv.Itoa(n)
		}
		number, err := resolveIssue(c, branch, arg)
		if err != nil {
			return fail(err)
		}
		doc, _, err := importGitHub(c, branch, number, false)
		if err != nil {
			return fail(err)
		}
		return compact(doc), false
	case "list_channels":
		ov, err := buildOverview(c)
		if err != nil {
			return fail(err)
		}
		return compact(ov.Channels), false
	case "list_labels":
		return compact(labelRows(c)), false
	case "define_label", "define_channel":
		section, one := "labels", "label"
		if name == "define_channel" {
			section, one = "channels", "channel"
		}
		defName, desc := str("name"), str("description")
		if defName == "" || desc == "" {
			return "name and description are required", true
		}
		err := mutateDefs(c, fmt.Sprintf("define %s %s", one, defName), func(d schema.Doc) error {
			defsSection(d, section)[defName] = map[string]any{"description": desc}
			return nil
		})
		if err != nil {
			return fail(err)
		}
		return one + " " + defName + " defined", false
	}
	return "unknown tool: " + name, true
}

func init() {
	rootCmd.AddCommand(mcpCmd)
}
