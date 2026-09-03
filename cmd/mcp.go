package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/robertdewilde-dev/git-track/internal/lock"
	"github.com/robertdewilde-dev/git-track/internal/schema"
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

func mcpTools() []map[string]any {
	branchProp := map[string]any{
		"type":        "string",
		"description": "Branch name. Omit to use the currently checked-out branch.",
	}
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
		tool("get_branch_context",
			"Read the full metadata document for a branch (issue number, title, state, context notes, next steps, labels, links, lock status). Returns JSON. Use this first to understand where work on a branch stands.",
			map[string]any{"branch": branchProp}),
		tool("get_context_markdown",
			"Read a branch's metadata as a prompt-ready markdown summary. Ideal for injecting branch context into a conversation.",
			map[string]any{"branch": branchProp}),
		tool("list_branches",
			"List every branch that has metadata, with its full document. Returns a JSON array of {branch, meta}.",
			map[string]any{}),
		tool("set_field",
			"Set one metadata field. Dot paths reach nested fields (e.g. agent.notes). Values are parsed as JSON where sensible (arrays like [\"a\",\"b\"], numbers), otherwise stored as strings. state is validated against the configured state set (default: todo, in-progress, blocked, review, done).",
			map[string]any{
				"branch": branchProp,
				"key":    map[string]any{"type": "string", "description": "Field name or dot path, e.g. state, title, context, next, agent.notes"},
				"value":  map[string]any{"type": "string", "description": "Value to store; JSON arrays/numbers are parsed"},
			}, "key", "value"),
		tool("unset_field",
			"Remove one metadata field by name or dot path.",
			map[string]any{
				"branch": branchProp,
				"key":    map[string]any{"type": "string", "description": "Field name or dot path"},
			}, "key"),
		tool("acquire_lock",
			"Acquire the distributed agent lock for a branch before making changes another agent might race on. The lock is enforced via a compare-and-swap push; if another actor holds it, this fails and you should work on something else or wait. Prefer setting a ttl so a crashed agent never wedges the branch.",
			map[string]any{
				"branch": branchProp,
				"ttl":    map[string]any{"type": "string", "description": "Auto-expiry duration like 30m or 2h (recommended)"},
				"force":  map[string]any{"type": "boolean", "description": "Steal a lock held by another actor"},
			}),
		tool("release_lock",
			"Release the agent lock for a branch when done working on it.",
			map[string]any{"branch": branchProp}),
		tool("say",
			"Post a message to an async channel shared across agents and machines. Without a channel it goes to the current branch's channel. Use it to leave findings, decisions, and progress notes for other agents (and your future self). The message syncs to the remote immediately; concurrent posts merge automatically.",
			map[string]any{
				"text":    map[string]any{"type": "string", "description": "The message body"},
				"channel": map[string]any{"type": "string", "description": "Channel name (e.g. a topic like 'android' or 'planning'). Omit for the current branch's channel."},
				"labels":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Labels classifying the message (e.g. bug, decision). See list_labels for the shared vocabulary."},
			}, "text"),
		tool("read_chat",
			"Read a channel's recent messages (newest first). Without a channel it reads the current branch's channel. Messages from other machines arrive via fetch; run `git track fetch` first for the latest.",
			map[string]any{
				"channel": map[string]any{"type": "string", "description": "Channel name. Omit for the current branch's channel."},
				"limit":   map[string]any{"type": "number", "description": "Max messages to return (default 20)"},
			}),
		tool("list_channels",
			"List channels: implicit per-branch channels that have messages, plus explicitly defined ones with their purpose.",
			map[string]any{}),
		tool("list_labels",
			"List the shared label vocabulary with each label's meaning. Labels classify both branch metadata (the labels field) and chat messages.",
			map[string]any{}),
		tool("define_label",
			"Define (or redefine) a label once with an explanation of what it means, shared across all branches and machines. Labels are optional documentation — undefined labels also work.",
			map[string]any{
				"name":        map[string]any{"type": "string", "description": "Label name, e.g. bug, android, decision"},
				"description": map[string]any{"type": "string", "description": "What the label means"},
			}, "name", "description"),
		tool("define_channel",
			"Define (or redefine) a named channel with its purpose, shared across branches and machines.",
			map[string]any{
				"name":        map[string]any{"type": "string", "description": "Channel name, e.g. planning"},
				"description": map[string]any{"type": "string", "description": "What the channel is for"},
			}, "name", "description"),
	}
}

// callMCPTool executes one tool. Errors are returned as text with isError so
// the agent can read the reason (e.g. "lock held by ...").
func callMCPTool(c *appCtx, name string, args map[string]any) (string, bool) {
	str := func(key string) string {
		v, _ := args[key].(string)
		return v
	}
	branch := str("branch")
	if branch == "" {
		var err error
		if branch, err = c.branch(); err != nil && name != "list_branches" {
			return err.Error(), true
		}
	}
	fail := func(err error) (string, bool) {
		return fmt.Sprintf("%s (exit code %d)", err.Error(), exitCodeFor(err)), true
	}
	switch name {
	case "get_branch_context":
		doc, _, err := c.store.Read(branch)
		if err != nil {
			return fail(err)
		}
		b, _ := doc.Marshal()
		return string(b), false
	case "get_context_markdown":
		doc, _, err := c.store.Read(branch)
		if err != nil {
			return fail(err)
		}
		msgs, _ := c.store.Messages(branch, 5)
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
		b, _ := json.MarshalIndent(rows, "", "  ")
		return string(b), false
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
		b, _ := doc.Marshal()
		return string(b), false
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
		if text == "" {
			return "empty message", true
		}
		channel := str("channel")
		if channel == "" {
			channel = branch
		}
		var labels []string
		if arr, ok := args["labels"].([]any); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					labels = append(labels, s)
				}
			}
		}
		sha, err := c.store.AppendMessage(channel, text, lock.Actor(), labels)
		if err != nil {
			return fail(err)
		}
		note := ""
		if err := c.store.SyncChannel(c.remote(), channel); err != nil {
			note = " (saved locally; remote sync failed: " + err.Error() + ")"
		}
		return "posted to #" + channel + " as " + sha[:12] + note, false
	case "read_chat":
		channel := str("channel")
		if channel == "" {
			channel = branch
		}
		limit := 20
		if n, ok := args["limit"].(float64); ok && n > 0 {
			limit = int(n)
		}
		msgs, err := c.store.Messages(channel, limit)
		if err != nil {
			return fail(err)
		}
		b, _ := json.MarshalIndent(map[string]any{"channel": channel, "messages": msgs}, "", "  ")
		return string(b), false
	case "list_channels", "list_labels":
		section := "channels"
		if name == "list_labels" {
			section = "labels"
		}
		defs, _, _ := c.store.ReadDefs()
		entries := map[string]any{}
		if defs != nil {
			if m, ok := defs[section].(map[string]any); ok {
				entries = m
			}
		}
		rows := []map[string]any{}
		for n, v := range entries {
			rows = append(rows, map[string]any{"name": n, "description": defDescription(v)})
		}
		if section == "channels" {
			existing, _ := c.store.Channels()
			for _, n := range existing {
				if _, ok := entries[n]; !ok {
					rows = append(rows, map[string]any{"name": n, "description": ""})
				}
			}
		}
		b, _ := json.MarshalIndent(rows, "", "  ")
		return string(b), false
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
