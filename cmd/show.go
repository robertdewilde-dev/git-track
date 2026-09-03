package cmd

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robertdewilde-dev/git-track/internal/lock"
	"github.com/robertdewilde-dev/git-track/internal/schema"
	"github.com/robertdewilde-dev/git-track/internal/store"
	"github.com/spf13/cobra"
)

var showCmd = &cobra.Command{
	Use:   "show [branch]",
	Short: "Human-readable summary of a branch's metadata",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ifExists, _ := cmd.Flags().GetBool("if-exists")
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
				if ifExists {
					return nil
				}
				return jsonError(err)
			}
		}
		doc, _, err := c.store.Read(branch)
		if err != nil {
			if ifExists && errors.Is(err, store.ErrNoMetadata) {
				return nil
			}
			return jsonError(err)
		}
		if flagJSON {
			return printJSON(map[string]any{"branch": branch, "meta": doc})
		}
		printSummary(branch, doc)
		return nil
	},
}

func printSummary(branch string, doc schema.Doc) {
	fmt.Printf("%s %s\n", bold("branch:"), branch)
	if issue, ok := doc["issue"].(float64); ok {
		title, _ := doc["title"].(string)
		fmt.Printf("%s  #%d %s\n", bold("issue:"), int64(issue), title)
	} else if title, ok := doc["title"].(string); ok {
		fmt.Printf("%s  %s\n", bold("title:"), title)
	}
	if state, ok := doc["state"].(string); ok {
		fmt.Printf("%s  %s\n", bold("state:"), state)
	}
	if ctx, ok := doc["context"].(string); ok && ctx != "" {
		fmt.Printf("%s %s\n", bold("context:"), ctx)
	}
	if next := stringList(doc["next"]); len(next) > 0 {
		fmt.Printf("%s\n", bold("next:"))
		for _, n := range next {
			fmt.Printf("  - %s\n", n)
		}
	}
	if labels := stringList(doc["labels"]); len(labels) > 0 {
		fmt.Printf("%s %s\n", bold("labels:"), strings.Join(labels, ", "))
	}
	if links := stringList(doc["links"]); len(links) > 0 {
		fmt.Printf("%s  %s\n", bold("links:"), strings.Join(links, " "))
	}
	if li := lock.FromDoc(doc); li != nil {
		status := ""
		if li.Expired(time.Now()) {
			status = " (expired)"
		} else if li.TTL > 0 {
			status = fmt.Sprintf(" (ttl %s)", li.TTL)
		}
		fmt.Printf("%s   %s%s\n", bold("lock:"), li.Owner, status)
	}
	updatedAt, _ := doc["updatedAt"].(string)
	updatedBy, _ := doc["updatedBy"].(string)
	if updatedAt != "" || updatedBy != "" {
		fmt.Printf("%s\n", dim(fmt.Sprintf("updated %s by %s", updatedAt, updatedBy)))
	}
}

func stringList(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func init() {
	showCmd.Flags().Bool("if-exists", false, "exit 0 silently when there is no metadata (used by hooks)")
	_ = showCmd.Flags().MarkHidden("if-exists")
	rootCmd.AddCommand(showCmd)
}
