// Package cmd implements the git-track CLI on top of internal/store.
package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/robertdewilde-dev/git-track/internal/gitcmd"
	"github.com/robertdewilde-dev/git-track/internal/lock"
	"github.com/robertdewilde-dev/git-track/internal/schema"
	"github.com/robertdewilde-dev/git-track/internal/store"
	"github.com/spf13/cobra"
)

// Stable exit codes — part of the integration contract (see SPEC.md).
const (
	ExitOK         = 0
	ExitError      = 1
	ExitNoMetadata = 2
	ExitLocked     = 3
	ExitTooNew     = 4
	ExitConflict   = 5
)

var (
	flagJSON    bool
	flagBranch  string
	flagQuiet   bool
	flagNoColor bool
)

// lockHeldError maps to exit code 3.
type lockHeldError struct{ owner string }

func (e *lockHeldError) Error() string {
	return fmt.Sprintf("locked by %s (use --force to override)", e.owner)
}

// codedError carries an explicit exit code.
type codedError struct {
	code int
	msg  string
}

func (e *codedError) Error() string { return e.msg }

func exitErr(code int, format string, a ...any) error {
	return &codedError{code: code, msg: fmt.Sprintf(format, a...)}
}

var rootCmd = &cobra.Command{
	Use:           "git-track",
	Short:         "Branch-scoped issue metadata stored in git refs",
	Long: `git-track stores per-branch issue metadata as commit objects under
refs/meta/branches/*. It syncs via normal git push/fetch and needs no forge or
external service. Every read command supports --json (machine output on
stdout, human messages on stderr; valid JSON even on error).

Exit codes (stable, see SPEC.md):
  0  success
  1  general error
  2  no metadata for this branch
  3  lock held by another actor
  4  metadata schemaVersion newer than this binary
  5  ref conflict (non-fast-forward) — run 'git track fetch' and retry`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.BoolVar(&flagJSON, "json", false, "machine-readable JSON output on stdout")
	pf.StringVar(&flagBranch, "branch", "", "operate on this branch instead of the current one")
	pf.BoolVarP(&flagQuiet, "quiet", "q", false, "suppress informational messages")
	pf.BoolVar(&flagNoColor, "no-color", false, "disable colored output")
	rootCmd.CompletionOptions.DisableDefaultCmd = true
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	err := rootCmd.Execute()
	if err == nil {
		return ExitOK
	}
	code := exitCodeFor(err)
	if msg := err.Error(); msg != "" && !flagQuiet {
		fmt.Fprintf(os.Stderr, "git-track: %s\n", msg)
	}
	return code
}

func exitCodeFor(err error) int {
	var ce *codedError
	if errors.As(err, &ce) {
		return ce.code
	}
	if errors.Is(err, store.ErrNoMetadata) {
		return ExitNoMetadata
	}
	var conflict *store.ConflictError
	if errors.As(err, &conflict) {
		return ExitConflict
	}
	var tooNew *schema.ErrTooNew
	if errors.As(err, &tooNew) {
		return ExitTooNew
	}
	var held *lockHeldError
	if errors.As(err, &held) {
		return ExitLocked
	}
	return ExitError
}

// appCtx bundles the repo handles every command needs.
type appCtx struct {
	git   *gitcmd.Runner
	store *store.Store
}

func newCtx() (*appCtx, error) {
	g := &gitcmd.Runner{}
	if _, err := g.Run("rev-parse", "--git-dir"); err != nil {
		return nil, exitErr(ExitError, "not a git repository")
	}
	return &appCtx{git: g, store: store.New(g)}, nil
}

// branch resolves the target branch: --branch flag, else HEAD.
func (c *appCtx) branch() (string, error) {
	if flagBranch != "" {
		return flagBranch, nil
	}
	name, err := c.git.Run("symbolic-ref", "--short", "--quiet", "HEAD")
	if err != nil || name == "" {
		return "", exitErr(ExitError, "HEAD is detached; use --branch <name>")
	}
	return name, nil
}

// remote resolves the metadata remote: track.remote config, else "origin".
func (c *appCtx) remote() string {
	if r := c.git.Config("track.remote"); r != "" {
		return r
	}
	return "origin"
}

// states returns the allowed state set (git config track.states, comma- or
// space-separated), defaulting to schema.DefaultStates.
func (c *appCtx) states() []string {
	raw := c.git.Config("track.states")
	if raw == "" {
		return schema.DefaultStates
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' })
	var states []string
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			states = append(states, f)
		}
	}
	return states
}

// mutateDoc runs the shared write path: read current state, enforce the lock,
// apply fn, stamp the auto fields, validate, and CAS-write the new commit.
func (c *appCtx) mutateDoc(branch string, force bool, message string, fn func(schema.Doc) error) (schema.Doc, error) {
	doc, parent, err := c.store.Read(branch)
	if errors.Is(err, store.ErrNoMetadata) {
		doc, parent = schema.New(), ""
	} else if err != nil {
		return nil, err
	}
	if err := doc.CheckWritable(); err != nil {
		return nil, err
	}
	if li := lock.Active(doc, time.Now()); li != nil && !lock.SameActor(li.Owner) && !force {
		return nil, &lockHeldError{owner: li.Owner}
	}
	if err := fn(doc); err != nil {
		return nil, err
	}
	stampAutoFields(doc)
	if err := schema.Validate(doc, c.states()); err != nil {
		var tooNew *schema.ErrTooNew
		if errors.As(err, &tooNew) {
			return nil, err
		}
		return nil, exitErr(ExitError, "%s", err)
	}
	if _, err := c.store.Write(branch, doc, parent, message); err != nil {
		return nil, err
	}
	return doc, nil
}

func stampAutoFields(doc schema.Doc) {
	now := time.Now().UTC().Format(time.RFC3339)
	doc["updatedAt"] = now
	doc["updatedBy"] = lock.Actor()
	if _, ok := doc["agent"].(map[string]any); ok {
		_ = doc.Set("agent.machine", lock.Machine())
	}
}

// printJSON writes v as JSON to stdout.
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// jsonError emits the contract error object ({"error": ..., "code": N}) on
// stdout so --json read commands stay valid JSON even on failure, then
// returns err for stderr reporting and the exit code.
func jsonError(err error) error {
	if flagJSON {
		_ = printJSON(map[string]any{"error": err.Error(), "code": exitCodeFor(err)})
	}
	return err
}

// info prints a human message to stderr unless --quiet.
func info(format string, a ...any) {
	if !flagQuiet {
		fmt.Fprintf(os.Stderr, format+"\n", a...)
	}
}

func useColor() bool {
	return !flagNoColor && os.Getenv("NO_COLOR") == ""
}

func bold(s string) string {
	if useColor() {
		return "\x1b[1m" + s + "\x1b[0m"
	}
	return s
}

func dim(s string) string {
	if useColor() {
		return "\x1b[2m" + s + "\x1b[0m"
	}
	return s
}
