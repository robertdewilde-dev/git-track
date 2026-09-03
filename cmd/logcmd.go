package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var logCmd = &cobra.Command{
	Use:   "log [branch]",
	Short: "History of metadata changes",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
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
		entries, err := c.store.Log(branch)
		if err != nil {
			return jsonError(err)
		}
		if flagJSON {
			return printJSON(entries)
		}
		for _, e := range entries {
			fmt.Printf("%s  %s  %s\n", dim(e.SHA[:8]), e.Date, e.Subject)
		}
		return nil
	},
}

var worktreeCmd = &cobra.Command{
	Use:   "worktree",
	Short: "Show metadata for every active worktree",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newCtx()
		if err != nil {
			return jsonError(err)
		}
		out, err := c.git.Run("worktree", "list", "--porcelain")
		if err != nil {
			return jsonError(err)
		}
		type wt struct {
			Path   string `json:"path"`
			Branch string `json:"branch"`
			Meta   any    `json:"meta"`
		}
		var trees []wt
		cur := wt{}
		flush := func() {
			if cur.Path != "" {
				trees = append(trees, cur)
			}
			cur = wt{}
		}
		for _, line := range splitLines(out) {
			switch {
			case line == "":
				flush()
			case hasPrefix(line, "worktree "):
				cur.Path = line[len("worktree "):]
			case hasPrefix(line, "branch refs/heads/"):
				cur.Branch = line[len("branch refs/heads/"):]
			}
		}
		flush()
		for i := range trees {
			if trees[i].Branch == "" {
				continue
			}
			if doc, _, err := c.store.Read(trees[i].Branch); err == nil {
				trees[i].Meta = doc
			}
		}
		if flagJSON {
			if trees == nil {
				trees = []wt{}
			}
			return printJSON(trees)
		}
		for i, t := range trees {
			if i > 0 {
				fmt.Println()
			}
			fmt.Printf("%s %s\n", bold("worktree:"), t.Path)
			if t.Branch == "" {
				info("  (detached HEAD)")
				continue
			}
			if doc, _, err := c.store.Read(t.Branch); err == nil {
				printSummary(t.Branch, doc)
			} else {
				fmt.Printf("%s %s %s\n", bold("branch:"), t.Branch, dim("(no metadata)"))
			}
		}
		return nil
	},
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	return append(lines, s[start:])
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func init() {
	rootCmd.AddCommand(logCmd, worktreeCmd)
}
