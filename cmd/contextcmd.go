package cmd

import (
	"fmt"
	"strings"

	"github.com/robertdewilde-dev/git-track/internal/schema"
	"github.com/spf13/cobra"
)

var contextCmd = &cobra.Command{
	Use:   "context [branch]",
	Short: "Export branch metadata as a markdown block for agent prompts",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		format, _ := cmd.Flags().GetString("format")
		if format != "markdown" {
			return exitErr(ExitError, "unsupported --format %q (only: markdown)", format)
		}
		c, err := newCtx()
		if err != nil {
			return jsonError(err)
		}
		branch := flagBranch
		if len(args) == 1 {
			branch = args[0]
		}
		if branch == "" {
			if branch, err = c.branch(); err != nil {
				return jsonError(err)
			}
		}
		doc, _, err := c.store.Read(branch)
		if err != nil {
			return jsonError(err)
		}
		md := contextMarkdown(branch, doc)
		if flagJSON {
			return printJSON(map[string]any{"branch": branch, "markdown": md})
		}
		fmt.Print(md)
		return nil
	},
}

// contextMarkdown renders a prompt-ready markdown block. Shared with the MCP
// server's get_context_markdown tool.
func contextMarkdown(branch string, doc schema.Doc) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Branch context: %s\n\n", branch)
	if n, ok := doc["issue"].(float64); ok {
		if title, ok := doc["title"].(string); ok {
			fmt.Fprintf(&b, "- **Issue:** #%d — %s\n", int64(n), title)
		} else {
			fmt.Fprintf(&b, "- **Issue:** #%d\n", int64(n))
		}
	} else if title, ok := doc["title"].(string); ok {
		fmt.Fprintf(&b, "- **Title:** %s\n", title)
	}
	if state, ok := doc["state"].(string); ok {
		fmt.Fprintf(&b, "- **State:** %s\n", state)
	}
	if ctx, ok := doc["context"].(string); ok && ctx != "" {
		fmt.Fprintf(&b, "- **Context:** %s\n", ctx)
	}
	if next := stringList(doc["next"]); len(next) > 0 {
		fmt.Fprintf(&b, "- **Next steps:**\n")
		for _, n := range next {
			fmt.Fprintf(&b, "  - %s\n", n)
		}
	}
	if tags := stringList(doc["tags"]); len(tags) > 0 {
		fmt.Fprintf(&b, "- **Tags:** %s\n", strings.Join(tags, ", "))
	}
	if links := stringList(doc["links"]); len(links) > 0 {
		fmt.Fprintf(&b, "- **Links:** %s\n", strings.Join(links, " "))
	}
	if notes, ok := doc.Get("agent.notes"); ok {
		if s, ok := notes.(string); ok && s != "" {
			fmt.Fprintf(&b, "- **Agent notes:** %s\n", s)
		}
	}
	updatedAt, _ := doc["updatedAt"].(string)
	updatedBy, _ := doc["updatedBy"].(string)
	if updatedAt != "" || updatedBy != "" {
		fmt.Fprintf(&b, "\n_Last updated %s by %s_\n", updatedAt, updatedBy)
	}
	return b.String()
}

func init() {
	contextCmd.Flags().String("format", "markdown", "output format")
	rootCmd.AddCommand(contextCmd)
}
