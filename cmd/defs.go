package cmd

// Shared label and channel definitions. Both vocabularies live in one
// document (defs.json at refs/meta/defs/all) so a single fetch shares them
// across branches and machines. Definitions are optional documentation:
// nothing enforces them — they exist so agents agree on what a label means.

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
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

func listDefs(c *appCtx, section string, extra map[string]int) error {
	doc, _, err := c.store.ReadDefs()
	entries := map[string]any{}
	if err == nil {
		if m, ok := doc[section].(map[string]any); ok {
			entries = m
		}
	}
	names := map[string]bool{}
	for n := range entries {
		names[n] = true
	}
	for n := range extra {
		names[n] = true
	}
	if len(names) == 0 {
		return jsonError(exitErr(ExitNoMetadata, "no %s defined", section))
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	if flagJSON {
		rows := []map[string]any{}
		for _, n := range sorted {
			row := map[string]any{"name": n, "description": defDescription(entries[n])}
			if extra != nil {
				row["messages"] = extra[n]
			}
			rows = append(rows, row)
		}
		return printJSON(rows)
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	if extra != nil {
		fmt.Fprintln(w, "CHANNEL\tMESSAGES\tDESCRIPTION")
		for _, n := range sorted {
			fmt.Fprintf(w, "%s\t%d\t%s\n", n, extra[n], defDescription(entries[n]))
		}
	} else {
		fmt.Fprintln(w, "LABEL\tDESCRIPTION")
		for _, n := range sorted {
			fmt.Fprintf(w, "%s\t%s\n", n, defDescription(entries[n]))
		}
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
	labelsCmd.Long = `Labels are a shared, optional vocabulary used to classify both branch
metadata (the "labels" field) and chat messages (git track say --label).
Defining a label records what it means, once, for every agent and machine.
Undefined labels still work — definitions are documentation, not enforcement.`
	labelsCmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := newCtx()
		if err != nil {
			return jsonError(err)
		}
		return listDefs(c, "labels", nil)
	}

	channelsCmd, _, _ := defineCmds("channels", "channel")
	channelsCmd.Long = `Channels are async message streams shared across branches and machines
(see git track say / chat). Every branch implicitly has one named after it;
named channels ("android", "planning") span branches. Defining a channel
records its purpose; posting to an undefined channel also just works.`
	channelsCmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := newCtx()
		if err != nil {
			return jsonError(err)
		}
		counts := map[string]int{}
		existing, _ := c.store.Channels()
		for _, name := range existing {
			counts[name] = 0
			if out, err := c.git.Run("rev-list", "--count", c.store.ChannelRef(name)); err == nil {
				if n, err := strconv.Atoi(out); err == nil {
					counts[name] = n
				}
			}
		}
		return listDefs(c, "channels", counts)
	}

	rootCmd.AddCommand(labelsCmd, channelsCmd)
}
