package cmd

// Shared label and channel definitions. Both vocabularies live in one
// document (defs.json at refs/meta/defs/all) so a single fetch shares them
// across branches and machines. Definitions are optional documentation:
// nothing enforces them — they exist so agents agree on what a label means.

import (
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/robertdewilde-dev/git-track/internal/schema"
	"github.com/robertdewilde-dev/git-track/internal/store"
	"github.com/spf13/cobra"
)

// readDefs returns the defs document (a fresh one if none exists) and its
// commit SHA ("" when new).
func readDefs(c *appCtx) (schema.Doc, string) {
	doc, sha, err := c.store.ReadDefs()
	if err != nil || doc == nil {
		return schema.Doc{"schemaVersion": float64(schema.CurrentVersion)}, ""
	}
	return doc, sha
}

// defsSection returns one section ("labels" or "channels") as a map,
// creating it in the doc when absent.
func defsSection(doc schema.Doc, section string) map[string]any {
	if m, ok := doc[section].(map[string]any); ok {
		return m
	}
	m := map[string]any{}
	doc[section] = m
	return m
}

// defDescription extracts the description of one definition entry.
func defDescription(v any) string {
	if m, ok := v.(map[string]any); ok {
		s, _ := m["description"].(string)
		return s
	}
	return ""
}

// mutateDefs applies fn to the defs document and syncs it to the remote.
// A push conflict (another machine defined something concurrently) is
// resolved by fetching their version and re-applying fn on top of it.
func mutateDefs(c *appCtx, message string, fn func(schema.Doc) error) error {
	for attempt := 0; attempt < 3; attempt++ {
		doc, parent := readDefs(c)
		if err := doc.CheckWritable(); err != nil {
			return err
		}
		if err := fn(doc); err != nil {
			return err
		}
		if _, err := c.store.WriteDefs(doc, parent, message); err != nil {
			return err
		}
		_, err := c.store.PushRef(c.remote(), c.store.DefsRef())
		if err == nil {
			return nil
		}
		var conflict *store.ConflictError
		if errors.As(err, &conflict) {
			if ferr := c.store.FetchDefs(c.remote()); ferr != nil {
				return ferr
			}
			continue // re-apply on top of the fetched version
		}
		info("definitions saved locally; sync failed (%s) — they will sync on `git track push --all`", err)
		return nil
	}
	return &store.ConflictError{Ref: c.store.DefsRef(), Detail: "could not sync definitions after retries"}
}

// hintUndefinedLabels nudges (stderr only) when a label is used that has no
// definition while a label vocabulary exists. Labels stay optional and free —
// this is a hint, never an error.
func hintUndefinedLabels(c *appCtx, labels []string) {
	if len(labels) == 0 {
		return
	}
	doc, _, err := c.store.ReadDefs()
	if err != nil {
		return
	}
	defined, ok := doc["labels"].(map[string]any)
	if !ok || len(defined) == 0 {
		return
	}
	for _, l := range labels {
		if _, ok := defined[l]; !ok {
			info("hint: label %q is not defined — describe it once with `git track labels set %s \"<what it means>\"`", l, l)
		}
	}
}

// labelRow is one label with its meaning and where it is used.
type labelRow struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Branches    int    `json:"branches"`
	Messages    int    `json:"messages"`
	Commits     int    `json:"commits"`
}

// labelRows lists every label that is defined or in use — on branch
// metadata, on chat messages, or as a "Label:" trailer on ordinary commits —
// with counts. Sorted by name.
func labelRows(c *appCtx) []labelRow {
	doc, _, err := c.store.ReadDefs()
	defined := map[string]any{}
	if err == nil {
		if m, ok := doc["labels"].(map[string]any); ok {
			defined = m
		}
	}
	branchUse := map[string]int{}
	if branches, err := c.store.Branches(); err == nil {
		for _, b := range branches {
			if d, _, err := c.store.Read(b); err == nil {
				for _, l := range stringList(d["labels"]) {
					branchUse[l]++
				}
			}
		}
	}
	msgUse, commitUse := c.store.LabelUsage()
	names := map[string]bool{}
	for _, m := range []map[string]int{branchUse, msgUse, commitUse} {
		for n := range m {
			names[n] = true
		}
	}
	for n := range defined {
		names[n] = true
	}
	rows := []labelRow{}
	for _, n := range sortedKeys(names) {
		rows = append(rows, labelRow{Name: n, Description: defDescription(defined[n]),
			Branches: branchUse[n], Messages: msgUse[n], Commits: commitUse[n]})
	}
	return rows
}

func listLabels(c *appCtx) error {
	rows := labelRows(c)
	if flagJSON {
		return printJSON(rows)
	}
	if len(rows) == 0 {
		return exitErr(ExitNoMetadata, "no labels defined or in use")
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "LABEL\tBRANCHES\tMESSAGES\tCOMMITS\tDESCRIPTION")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\n", r.Name, r.Branches, r.Messages, r.Commits, r.Description)
	}
	return w.Flush()
}

func listChannels(c *appCtx, counts map[string]int) error {
	doc, _, err := c.store.ReadDefs()
	entries := map[string]any{}
	if err == nil {
		if m, ok := doc["channels"].(map[string]any); ok {
			entries = m
		}
	}
	if _, ok := entries[store.MainChannel]; !ok {
		entries[store.MainChannel] = map[string]any{"description": "Shared coordination channel: questions, fan-out, announcements — anything not tied to one branch"}
	}
	names := map[string]bool{}
	for n := range entries {
		names[n] = true
	}
	for n := range counts {
		names[n] = true
	}
	sorted := sortedKeys(names)
	if flagJSON {
		rows := []map[string]any{}
		for _, n := range sorted {
			rows = append(rows, map[string]any{"name": n, "description": defDescription(entries[n]), "messages": counts[n]})
		}
		return printJSON(rows)
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "CHANNEL\tMESSAGES\tDESCRIPTION")
	for _, n := range sorted {
		fmt.Fprintf(w, "%s\t%d\t%s\n", n, counts[n], defDescription(entries[n]))
	}
	return w.Flush()
}

func defineCmds(section, one string) (*cobra.Command, *cobra.Command, *cobra.Command) {
	list := &cobra.Command{
		Use:   section,
		Short: fmt.Sprintf("List defined %s (a shared, optional vocabulary)", section),
		Args:  cobra.NoArgs,
	}
	set := &cobra.Command{
		Use:   "set <name> <description>",
		Short: fmt.Sprintf("Define a %s once, with an explanation of what it means", one),
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newCtx()
			if err != nil {
				return jsonError(err)
			}
			err = mutateDefs(c, fmt.Sprintf("define %s %s", one, args[0]), func(d schema.Doc) error {
				defsSection(d, section)[args[0]] = map[string]any{"description": args[1]}
				return nil
			})
			if err != nil {
				return jsonError(err)
			}
			if flagJSON {
				return printJSON(map[string]any{"name": args[0], "description": args[1]})
			}
			info("%s %q defined", one, args[0])
			return nil
		},
	}
	unset := &cobra.Command{
		Use:   "unset <name>",
		Short: fmt.Sprintf("Remove a %s definition (existing uses are untouched)", one),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newCtx()
			if err != nil {
				return jsonError(err)
			}
			err = mutateDefs(c, fmt.Sprintf("undefine %s %s", one, args[0]), func(d schema.Doc) error {
				m := defsSection(d, section)
				if _, ok := m[args[0]]; !ok {
					return exitErr(ExitNoMetadata, "no such %s: %s", one, args[0])
				}
				delete(m, args[0])
				return nil
			})
			if err != nil {
				return jsonError(err)
			}
			info("%s %q removed", one, args[0])
			return nil
		},
	}
	list.AddCommand(set, unset)
	return list, set, unset
}

func init() {
	labelsCmd, _, _ := defineCmds("labels", "label")
	labelsCmd.Long = `Labels are a shared, optional vocabulary used to classify branch metadata
(the "labels" field), chat messages (git track say --label), and ordinary
git commits (a "Label: <name>" trailer, e.g. git commit --trailer "Label: bug").
Defining a label records what it means, once, for every agent and machine.
Undefined labels still work — definitions are documentation, not enforcement.
The list shows where each label is used; 'labels show <name>' lists the uses.`
	labelsCmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := newCtx()
		if err != nil {
			return jsonError(err)
		}
		return listLabels(c)
	}
	labelsCmd.AddCommand(&cobra.Command{
		Use:   "show <name>",
		Short: "Everything carrying a label: branches, messages, commits (same as search --label)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newCtx()
			if err != nil {
				return jsonError(err)
			}
			return searchAndPrint(c, "", args[0], 0)
		},
	})

	channelsCmd, _, _ := defineCmds("channels", "channel")
	channelsCmd.Long = `Channels are async message streams shared across branches and machines
(see git track say / chat / watch). Every repository has "main" — the shared
coordination channel where anything can land: questions, fan-out,
announcements. Every branch has its own channel (branches/<branch>), and
named channels ("android", "planning") span branches. Defining a channel
records its purpose; posting to an undefined channel also just works.`
	channelsCmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := newCtx()
		if err != nil {
			return jsonError(err)
		}
		counts := map[string]int{store.MainChannel: 0}
		existing, _ := c.store.Channels()
		for _, name := range existing {
			counts[name] = c.store.MessageCount(name)
		}
		return listChannels(c, counts)
	}
	channelsCmd.AddCommand(&cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a channel and its messages, locally and on the remote",
		Long: `Delete a channel: the ref is removed locally and on the remote, so its
messages disappear for everyone who fetches. No consensus is required — it
is a plain ref delete by anyone with push access. Other clones keep their
local copy until they delete it too (a later post from such a clone
recreates the channel with its old history). The definition, if any, stays;
remove it with 'channels unset'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newCtx()
			if err != nil {
				return jsonError(err)
			}
			if err := c.store.DeleteChannel(c.remote(), args[0]); err != nil {
				return jsonError(err)
			}
			if flagJSON {
				return printJSON(map[string]any{"deleted": args[0]})
			}
			info("channel %q deleted", args[0])
			return nil
		},
	})

	rootCmd.AddCommand(labelsCmd, channelsCmd)
}
