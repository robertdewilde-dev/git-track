// Package gitcmd is a thin wrapper over git invocations. Every git call the
// tool makes goes through this package so the transport is swappable (e.g. for
// go-git) and testable in one place.
package gitcmd

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes git commands in a fixed working directory.
type Runner struct {
	// Dir is the directory git runs in. Empty means the process cwd.
	Dir string
}

// Error is a failed git invocation, carrying the exit code and stderr.
type Error struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = fmt.Sprintf("exit code %d", e.Code)
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), msg)
}

// Run executes git with args and returns trimmed stdout.
func (r *Runner) Run(args ...string) (string, error) {
	return r.RunStdin("", args...)
}

// RunStdin executes git with args, feeding stdin, and returns trimmed stdout.
func (r *Runner) RunStdin(stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		code := 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return strings.TrimRight(out.String(), "\n"), &Error{Args: args, Code: code, Stderr: errb.String()}
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// Config returns a git config value, or "" if unset.
func (r *Runner) Config(key string) string {
	out, err := r.Run("config", "--get", key)
	if err != nil {
		return ""
	}
	return out
}

// ConfigAll returns all values for a multi-valued config key.
func (r *Runner) ConfigAll(key string) []string {
	out, err := r.Run("config", "--get-all", key)
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}
