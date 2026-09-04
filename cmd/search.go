package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/robertdewilde-dev/git-track/internal/schema"
	"github.com/robertdewilde-dev/git-track/internal/store"
	"github.com/spf13/cobra"
)

// searchResult is what `search` (and MCP `search`, and `labels show`) return:
// the three places a label or a phrase can live.
type searchResult struct {
	Text     string                 `json:"text,omitempty"`
	Label    string                 `json:"label,omitempty"`
	Branches []searchBranch         `json:"branches"`
	Messages []store.ChannelMessage `json:"messages"`
	Commits  []store.Commit         `json:"commits"`
}

type searchBranch struct {
	Branch  string   `json:"branch"`
	State   string   `json:"state,omitempty"`
	Title   string   `json:"title,omitempty"`
	Labels  []string `json:"labels,omitempty"`
	Matched string   `json:"matched,omitempty"` // which field matched the text
}

// runSearch looks for text (case-insensitive substring) and/or a label across
// branch metadata, chat messages in every channel, and — for labels —
// ordinary git commits carrying a "Label:" trailer.
func runSearch(c *appCtx, text, label string, limit int) (*searchResult, error) {
	if text == "" && label == "" {
		return nil, exitErr(ExitError, "search needs text or --label")
	}
	res := &searchResult{Text: text, Label: label, Branches: []searchBranch{}, Messages: []store.ChannelMessage{}, Commits: []store.Commit{}}
	branches, _ := c.store.Branches()
	for _, b := range branches {
		doc, _, err := c.store.Read(b)
		if err != nil {
			continue
		}
		labels := stringList(doc["labels"])
		if label != "" && !containsStr(labels, label) {
			continue
		}
		matched := ""
		if text != "" {
			if matched = docMatch(doc, strings.ToLower(text)); matched == "" {
				continue
			}
		}
		row := searchBranch{Branch: b, Labels: labels, Matched: matched}
		row.State, _ = doc["state"].(string)
		row.Title, _ = doc["title"].(string)
		res.Branches = append(res.Branches, row)
	}
	msgs, err := c.store.SearchMessages(text, label, limit)
	if err != nil {
		return nil, err
	}
	if msgs != nil {
		res.Messages = msgs
	}
	if label != "" {
		commits, err := c.store.CommitsWithLabel(label, limit)
		if err != nil {
			return nil, err
		}
		for _, cm := range commits {
			if text == "" || strings.Contains(strings.ToLower(cm.Subject), strings.ToLower(text)) {
				res.Commits = append(res.Commits, cm)
			}
		}
	}
	return res, nil
}

// docMatch returns the dot path of the first string field whose lowercased
// value contains needle, or "".
func docMatch(doc schema.Doc, needle string) string {
	var walk func(prefix string, v any) string
	walk = func(prefix string, v any) string {
		switch x := v.(type) {
		case string:
			if strings.Contains(strings.ToLower(x), needle) {
				return prefix
			}
		case []any:
			for _, e := range x {
				if hit := walk(prefix, e); hit != "" {
					return hit
				}
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				e := x[k]
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				if hit := walk(p, e); hit != "" {
					return hit
				}
			}
		}
		return ""
	}
	return walk("", map[string]any(doc))
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func printSearch(res *searchResult) error {
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	if len(res.Branches) > 0 {
		fmt.Fprintln(w, "BRANCH\tSTATE\tTITLE\tLABELS\tMATCHED")
		for _, b := range res.Branches {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", b.Branch, b.State, b.Title, strings.Join(b.Labels, ","), b.Matched)
		}
	}
	if len(res.Messages) > 0 {
		fmt.Fprintln(w, "CHANNEL\tSHA\tWHEN\tBY\tLABELS\tMESSAGE")
		for _, m := range res.Messages {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\n", m.Channel, m.SHA[:7], ago(m.At), m.By, strings.Join(m.Labels, ","), firstLine(m.Body, 70))
		}
	}
	if len(res.Commits) > 0 {
		fmt.Fprintln(w, "COMMIT\tREF\tWHEN\tBY\tLABELS\tSUBJECT")
		for _, cm := range res.Commits {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\n", cm.SHA[:7], cm.Ref, ago(cm.At), cm.By, strings.Join(cm.Labels, ","), firstLine(cm.Subject, 70))
		}
	}
	return w.Flush()
}

func searchAndPrint(c *appCtx, text, label string, limit int) error {
	res, err := runSearch(c, text, label, limit)
	if err != nil {
		return jsonError(err)
	}
	if flagJSON {
		return printJSON(res)
	}
	if len(res.Branches)+len(res.Messages)+len(res.Commits) == 0 {
		what := fmt.Sprintf("%q", text)
		if label != "" {
			what = "label " + label
		}
		return exitErr(ExitNoMetadata, "nothing found for %s", what)
	}
	return printSearch(res)
}

var searchCmd = &cobra.Command{
	Use:   "search [text]",
	Short: "Find text or a label across branch metadata, chat, and commits",
	Long: `Search everything git-track knows without reading it all: branch metadata
(any string field), chat messages in every channel, and — with --label —
ordinary git commits whose message carries a "Label: <name>" trailer
(add one with: git commit --trailer "Label: bug").

Text matches are case-insensitive substrings. Combine text and --label to
narrow. 'git track labels show <name>' is the same as 'search --label <name>'.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		label, _ := cmd.Flags().GetString("label")
		limit, _ := cmd.Flags().GetInt("limit")
		c, err := newCtx()
		if err != nil {
			return jsonError(err)
		}
		text := ""
		if len(args) == 1 {
			text = args[0]
		}
		return searchAndPrint(c, text, label, limit)
	},
}

func init() {
	searchCmd.Flags().String("label", "", "only items carrying this label")
	searchCmd.Flags().IntP("limit", "n", 50, "max messages/commits to return (0 = all)")
	rootCmd.AddCommand(searchCmd)
}
