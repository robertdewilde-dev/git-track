package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/robertdewilde-dev/git-track/internal/lock"
	"github.com/robertdewilde-dev/git-track/internal/schema"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "Table of all branches with metadata",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newCtx()
		if err != nil {
			return jsonError(err)
		}
		branches, err := c.store.Branches()
		if err != nil {
			return jsonError(err)
		}
		type row struct {
			Branch string     `json:"branch"`
			Meta   schema.Doc `json:"meta"`
		}
		var rows []row
		for _, b := range branches {
			doc, _, err := c.store.Read(b)
			if err != nil {
				continue // fail safe: a broken ref never blocks the listing
			}
			rows = append(rows, row{Branch: b, Meta: doc})
		}
		if flagJSON {
			if rows == nil {
				rows = []row{}
			}
			return printJSON(rows)
		}
		if len(rows) == 0 {
			info("no branch metadata found")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "BRANCH\tSTATE\tISSUE\tTITLE\tLOCK\tUPDATED")
		for _, r := range rows {
			state, _ := r.Meta["state"].(string)
			title, _ := r.Meta["title"].(string)
			issue := ""
			if n, ok := r.Meta["issue"].(float64); ok {
				issue = fmt.Sprintf("#%d", int64(n))
			}
			locked := ""
			if li := lock.Active(r.Meta, time.Now()); li != nil {
				locked = li.Owner
			}
			updated, _ := r.Meta["updatedAt"].(string)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Branch, state, issue, title, locked, updated)
		}
		return w.Flush()
	},
}

func init() {
	rootCmd.AddCommand(listCmd)
}
