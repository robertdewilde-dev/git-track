package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"

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
			"Read the full metadata document for a branch (issue number, title, state, context notes, next steps, tags, links, lock status). Returns JSON. Use this first to understand where work on a branch stands.",
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
		return contextMarkdown(branch, doc), false
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
	}
	return "unknown tool: " + name, true
}

func init() {
	rootCmd.AddCommand(mcpCmd)
}
