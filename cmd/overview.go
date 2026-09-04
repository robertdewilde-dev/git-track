package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/robertdewilde-dev/git-track/internal/lock"
	"github.com/robertdewilde-dev/git-track/internal/store"
	"github.com/spf13/cobra"
)

// overview is the one-call orientation for an agent: every branch with
// metadata, every channel with its unread count, and the label vocabulary —
// generated on demand from the refs (nothing is stored), compact enough to
// read in a couple of hundred tokens.
type overview struct {
	Branch   string            `json:"branch,omitempty"`
	Branches []overviewBranch  `json:"branches"`
	Channels []overviewChannel `json:"channels"`
	Labels   []overviewLabel   `json:"labels"`
}

type overviewBranch struct {
	Branch    string   `json:"branch"`
	State     string   `json:"state,omitempty"`
	Issue     int64    `json:"issue,omitempty"`
	Title     string   `json:"title,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Lock      string   `json:"lock,omitempty"`
	UpdatedAt string   `json:"updatedAt,omitempty"`
}

type overviewChannel struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Messages    int            `json:"messages"`
	Unread      int            `json:"unread"`
	Last        *store.Message `json:"last,omitempty"`
}

type overviewLabel struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

func buildOverview(c *appCtx) (*overview, error) {
	ov := &overview{Branches: []overviewBranch{}, Channels: []overviewChannel{}, Labels: []overviewLabel{}}
	if b, err := c.branch(); err == nil {
		ov.Branch = b
	}
	branches, err := c.store.Branches()
	if err != nil {
		return nil, err
	}
	for _, b := range branches {
		doc, _, err := c.store.Read(b)
		if err != nil {
			continue
		}
		row := overviewBranch{Branch: b, Labels: stringList(doc["labels"])}
		row.State, _ = doc["state"].(string)
		row.Title, _ = doc["title"].(string)
		row.UpdatedAt, _ = doc["updatedAt"].(string)
		if n, ok := doc["issue"].(float64); ok {
			row.Issue = int64(n)
		}
		if li := lock.Active(doc, time.Now()); li != nil {
			row.Lock = li.Owner
		}
		ov.Branches = append(ov.Branches, row)
	}
	defs, _, _ := c.store.ReadDefs()
	descs := func(section string) map[string]any {
		if defs != nil {
			if m, ok := defs[section].(map[string]any); ok {
				return m
			}
		}
		return map[string]any{}
	}
	channelDefs, labelDefs := descs("channels"), descs("labels")
	names := map[string]bool{store.MainChannel: true}
	existing, _ := c.store.Channels()
	for _, n := range existing {
		names[n] = true
	}
	for n := range channelDefs {
		names[n] = true
	}
	for _, n := range sortedKeys(names) {
		ch := overviewChannel{Name: n, Description: defDescription(channelDefs[n])}
		if n == store.MainChannel && ch.Description == "" {
			ch.Description = "shared coordination"
		}
		ch.Messages = c.store.MessageCount(n)
		ch.Unread = c.store.Unread(n)
		if msgs, _ := c.store.Messages(n, 1); len(msgs) == 1 {
			ch.Last = &msgs[0]
		}
		ov.Channels = append(ov.Channels, ch)
	}
	labels := map[string]bool{}
	for n := range labelDefs {
		labels[n] = true
	}
	for _, n := range sortedKeys(labels) {
		ov.Labels = append(ov.Labels, overviewLabel{Name: n, Description: defDescription(labelDefs[n])})
	}
	return ov, nil
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ago renders an RFC 3339 time as a short relative age ("3m", "2h", "5d").
func ago(rfc string) string {
	t, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		return rfc
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// firstLine truncates s to its first line, at most n runes.
func firstLine(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + "…"
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

var overviewCmd = &cobra.Command{
	Use:   "overview",
	Short: "One-screen digest: branches, channels with unread counts, labels",
	Long: `Print a compact digest of everything git-track knows about this repository:
every branch with metadata (state, issue, title, lock), every channel with its
message and unread count and last message, and the label vocabulary. Meant as
the first call of an agent session — one read instead of four. Reads local
refs only; run 'git track fetch' first for a fresh view.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newCtx()
		if err != nil {
			return jsonError(err)
		}
		ov, err := buildOverview(c)
		if err != nil {
			return jsonError(err)
		}
		if flagJSON {
			return printJSON(ov)
		}
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "BRANCH\tSTATE\tISSUE\tTITLE\tLABELS\tLOCK\tUPDATED")
		for _, b := range ov.Branches {
			mark := "  "
			if b.Branch == ov.Branch {
				mark = "* "
			}
			issue := ""
			if b.Issue != 0 {
				issue = fmt.Sprintf("#%d", b.Issue)
			}
			fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\t%s\t%s\t%s\n", mark, b.Branch, b.State, issue, b.Title,
				strings.Join(b.Labels, ","), b.Lock, ago(b.UpdatedAt))
		}
		if len(ov.Branches) == 0 {
			fmt.Fprintln(w, "  (no branch metadata yet)")
		}
		fmt.Fprintln(w, "\n"+"CHANNEL\tMSGS\tUNREAD\tLAST\tDESCRIPTION")
		for _, ch := range ov.Channels {
			last := ""
			if ch.Last != nil {
				last = fmt.Sprintf("%s %s: %s", ago(ch.Last.At), ch.Last.By, firstLine(ch.Last.Body, 50))
			}
			fmt.Fprintf(w, "  %s\t%d\t%d\t%s\t%s\n", ch.Name, ch.Messages, ch.Unread, last, ch.Description)
		}
		fmt.Fprintln(w, "\n"+"LABEL\tDESCRIPTION")
		for _, l := range ov.Labels {
			fmt.Fprintf(w, "  %s\t%s\n", l.Name, l.Description)
		}
		if len(ov.Labels) == 0 {
			fmt.Fprintln(w, "  (none defined)")
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Println(dim("\nread new: git track chat <channel> --unread · find: git track search <text> | --label <name>"))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(overviewCmd)
}
