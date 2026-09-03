package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/robertdewilde-dev/git-track/internal/schema"
	"github.com/spf13/cobra"
)

// Fields whose CLI values are always taken as literal strings, so a title of
// "42" or a state of "null" is never coerced into a JSON scalar.
var stringFields = map[string]bool{
	"title": true, "state": true, "context": true,
	"updatedAt": true, "updatedBy": true,
	"agent.lockedBy": true, "agent.notes": true,
	"agent.machine": true, "agent.lastRun": true,
	"agent.lockedAt": true, "agent.lockTtl": true,
}

var setCmd = &cobra.Command{
	Use:   "set <key> <value> | set --from-json <file|->",
	Short: "Set a field (dot paths allowed) or replace the whole document",
	Args:  cobra.RangeArgs(0, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		fromJSON, _ := cmd.Flags().GetString("from-json")
		force, _ := cmd.Flags().GetBool("force")
		c, err := newCtx()
		if err != nil {
			return err
		}
		branch, err := c.branch()
		if err != nil {
			return err
		}

		if fromJSON != "" {
			if len(args) != 0 {
				return exitErr(ExitError, "--from-json takes no positional arguments")
			}
			return setFromJSON(c, branch, fromJSON, force)
		}
		if len(args) != 2 {
			return exitErr(ExitError, "usage: git track set <key> <value>")
		}
		key, raw := args[0], args[1]
		if key == "schemaVersion" {
			return exitErr(ExitError, "schemaVersion is managed by git-track and cannot be set")
		}
		doc, err := c.mutateDoc(branch, force, fmt.Sprintf("set %s", key), func(d schema.Doc) error {
			return d.Set(key, parseValue(key, raw))
		})
		if err != nil {
			return err
		}
		if flagJSON {
			return printJSON(doc)
		}
		info("%s: set %s", branch, key)
		return nil
	},
}

func setFromJSON(c *appCtx, branch, path string, force bool) error {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return exitErr(ExitError, "reading %s: %s", path, err)
	}
	replacement, err := schema.Parse(data)
	if err != nil {
		return exitErr(ExitError, "%s", err)
	}
	if err := replacement.CheckWritable(); err != nil {
		return err
	}
	doc, err := c.mutateDoc(branch, force, "replace document", func(d schema.Doc) error {
		for k := range d {
			delete(d, k)
		}
		for k, v := range replacement {
			d[k] = v
		}
		return nil
	})
	if err != nil {
		return err
	}
	if flagJSON {
		return printJSON(doc)
	}
	info("%s: document replaced", branch)
	return nil
}

// parseValue interprets a CLI value: known string fields stay strings;
// otherwise JSON scalars/arrays/objects are decoded, with plain-string
// fallback.
func parseValue(key, raw string) any {
	if stringFields[key] {
		return raw
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err == nil {
		return v
	}
	return raw
}

var unsetCmd = &cobra.Command{
	Use:   "unset <key>",
	Short: "Remove a field",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		force, _ := cmd.Flags().GetBool("force")
		c, err := newCtx()
		if err != nil {
			return err
		}
		branch, err := c.branch()
		if err != nil {
			return err
		}
		key := args[0]
		if key == "schemaVersion" {
			return exitErr(ExitError, "schemaVersion cannot be removed")
		}
		doc, err := c.mutateDoc(branch, force, fmt.Sprintf("unset %s", key), func(d schema.Doc) error {
			if !d.Unset(key) {
				return exitErr(ExitError, "no such field: %s", key)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if flagJSON {
			return printJSON(doc)
		}
		info("%s: unset %s", branch, key)
		return nil
	},
}

func init() {
	setCmd.Flags().String("from-json", "", "replace the whole document from a JSON file ('-' for stdin)")
	setCmd.Flags().Bool("force", false, "write even if locked by another actor")
	unsetCmd.Flags().Bool("force", false, "write even if locked by another actor")
	rootCmd.AddCommand(setCmd, unsetCmd)
}
