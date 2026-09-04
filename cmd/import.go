package cmd

// import: pull one GitHub issue's facts into this branch's metadata. One-way,
// per branch, a refresh on re-run. Talks to GitHub through the `gh` CLI so
// the core stays free of tokens, HTTP, and SDKs; only this command needs gh.

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/robertdewilde-dev/git-track/internal/schema"
	"github.com/spf13/cobra"
)

// ghIssue is the subset of `gh issue view --json` we map.
type ghIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"` // OPEN / CLOSED
	URL    string `json:"url"`
	Labels []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"labels"`
}

// gh runs the GitHub CLI and returns its stdout.
func gh(c *appCtx, args ...string) (string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", exitErr(ExitError, "import needs the GitHub CLI (gh) on PATH: https://cli.github.com")
	}
	cmd := exec.Command("gh", args...)
	cmd.Dir = c.git.Dir
	out, err := cmd.Output()
	if err != nil {
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", exitErr(ExitError, "gh %s: %s", strings.Join(args[:2], " "), msg)
	}
	return string(out), nil
}

var branchIssueRe = regexp.MustCompile(`(?:^|[/_-])#?(\d+)(?:[/_-]|$)`)

// resolveIssue picks the issue number: explicit argument, the branch's issue
// field, a number in the branch name, or the issue the branch's PR closes.
func resolveIssue(c *appCtx, branch, arg string) (int, error) {
	if arg != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
		if err != nil || n <= 0 {
			return 0, exitErr(ExitError, "not an issue number: %s", arg)
		}
		return n, nil
	}
	if doc, _, err := c.store.Read(branch); err == nil {
		if n, ok := doc["issue"].(float64); ok && n > 0 {
			return int(n), nil
		}
	}
	if m := branchIssueRe.FindStringSubmatch(branch); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n, nil
	}
	out, err := gh(c, "pr", "view", branch, "--json", "closingIssuesReferences")
	if err == nil {
		var pr struct {
			Closing []struct{ Number int } `json:"closingIssuesReferences"`
		}
		if json.Unmarshal([]byte(out), &pr) == nil && len(pr.Closing) > 0 {
			return pr.Closing[0].Number, nil
		}
	}
	return 0, exitErr(ExitNoMetadata, "no issue found for branch %q: pass a number (git track import 42)", branch)
}

// importGitHub fetches the issue and writes it into the branch document.
// Mapping: number→issue, title→title, body→context, labels→labels,
// url→links (appended), CLOSED→state done, OPEN→state todo only when unset.
// Fields GitHub does not know about are left alone.
func importGitHub(c *appCtx, branch string, number int, force bool) (schema.Doc, *ghIssue, error) {
	out, err := gh(c, "issue", "view", strconv.Itoa(number), "--json", "number,title,body,state,labels,url")
	if err != nil {
		return nil, nil, err
	}
	var issue ghIssue
	if err := json.Unmarshal([]byte(out), &issue); err != nil {
		return nil, nil, exitErr(ExitError, "unexpected gh output: %s", err)
	}
	var labels []any
	for _, l := range issue.Labels {
		labels = append(labels, l.Name)
	}
	states := c.states()
	doc, err := c.mutateDoc(branch, force, fmt.Sprintf("import github #%d", issue.Number), func(d schema.Doc) error {
		d["issue"] = float64(issue.Number)
		d["title"] = issue.Title
		if body := strings.TrimSpace(issue.Body); body != "" {
			d["context"] = body
		}
		if len(labels) > 0 {
			d["labels"] = labels
		} else {
			delete(d, "labels")
		}
		if issue.URL != "" && !slices.Contains(stringList(d["links"]), issue.URL) {
			links := []any{}
			for _, l := range stringList(d["links"]) {
				links = append(links, l)
			}
			d["links"] = append(links, issue.URL)
		}
		if issue.State == "CLOSED" && slices.Contains(states, "done") {
			d["state"] = "done"
		} else if _, has := d["state"]; !has && slices.Contains(states, "todo") {
			d["state"] = "todo"
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	// GitHub label descriptions become shared definitions, once, for labels
	// that have none yet.
	defs, _, _ := c.store.ReadDefs()
	defined := map[string]any{}
	if defs != nil {
		if m, ok := defs["labels"].(map[string]any); ok {
			defined = m
		}
	}
	missing := map[string]string{}
	for _, l := range issue.Labels {
		if _, ok := defined[l.Name]; !ok && strings.TrimSpace(l.Description) != "" {
			missing[l.Name] = l.Description
		}
	}
	if len(missing) > 0 {
		if err := mutateDefs(c, fmt.Sprintf("import github label definitions for #%d", issue.Number), func(d schema.Doc) error {
			sec := defsSection(d, "labels")
			for name, desc := range missing {
				if _, ok := sec[name]; !ok {
					sec[name] = map[string]any{"description": desc}
				}
			}
			return nil
		}); err != nil {
			info("label definitions not saved: %s", err)
		}
	}
	return doc, &issue, nil
}

var importCmd = &cobra.Command{
	Use:   "import [issue]",
	Short: "Pull a GitHub issue's title, body, labels, and state into this branch",
	Long: `Import one GitHub issue into the current branch's metadata (one-way; run
again to refresh). Needs the GitHub CLI (gh), authenticated; nothing else in
git-track does.

Which issue: the number you pass, else the branch's existing issue field,
else a number in the branch name (42-fix-auth, feat/42-x), else the issue
the branch's open pull request closes.

Mapping: number → issue, title → title, body → context, labels → labels,
url → links, CLOSED → state done (OPEN sets todo only when state is unset).
GitHub label descriptions become shared label definitions for labels not
defined yet. Fields GitHub knows nothing about (next, agent.*, ...) are
untouched.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		from, _ := cmd.Flags().GetString("from")
		force, _ := cmd.Flags().GetBool("force")
		if from != "github" {
			return jsonError(exitErr(ExitError, "unsupported --from %q (only: github)", from))
		}
		c, err := newCtx()
		if err != nil {
			return jsonError(err)
		}
		branch, err := c.branch()
		if err != nil {
			return jsonError(err)
		}
		arg := ""
		if len(args) == 1 {
			arg = args[0]
		}
		number, err := resolveIssue(c, branch, arg)
		if err != nil {
			return jsonError(err)
		}
		doc, issue, err := importGitHub(c, branch, number, force)
		if err != nil {
			return jsonError(err)
		}
		if flagJSON {
			return printJSON(doc)
		}
		state, _ := doc["state"].(string)
		info("imported #%d %q into %s (%d labels, state %s)", issue.Number, issue.Title, branch, len(issue.Labels), state)
		return nil
	},
}

func init() {
	importCmd.Flags().String("from", "github", "source (only github for now)")
	importCmd.Flags().Bool("force", false, "write even if locked by another actor")
	rootCmd.AddCommand(importCmd)
}
