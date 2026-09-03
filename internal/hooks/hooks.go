// Package hooks installs and removes the git-track hooks (post-checkout,
// pre-push). An existing hook is never overwritten: it is renamed to
// <name>.pre-git-track and the installed script chains to it first.
package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/robertdewilde-dev/git-track/internal/gitcmd"
)

// Marker identifies scripts installed by git-track.
const Marker = "# git-track hook v1"

// Names are the hooks git-track installs.
var Names = []string{"post-checkout", "pre-push"}

const postCheckoutScript = `#!/bin/sh
` + Marker + ` -- installed by 'git track init'; remove with 'git track init --undo'
chained="$(dirname "$0")/post-checkout.pre-git-track"
if [ -x "$chained" ]; then
  "$chained" "$@" || exit $?
fi
# On branch checkouts, print the metadata summary. Never block the checkout.
if [ "$3" = "1" ] && command -v git-track >/dev/null 2>&1; then
  git-track show --if-exists 2>/dev/null || true
fi
exit 0
`

const prePushScript = `#!/bin/sh
` + Marker + ` -- installed by 'git track init'; remove with 'git track init --undo'
stdin_data=$(cat)
chained="$(dirname "$0")/pre-push.pre-git-track"
if [ -x "$chained" ]; then
  if [ -n "$stdin_data" ]; then
    printf '%s\n' "$stdin_data" | "$chained" "$@" || exit $?
  else
    "$chained" "$@" </dev/null || exit $?
  fi
fi
# Push metadata refs alongside; non-fatal so a broken ref never blocks a push.
# When the outgoing push already carries metadata refs (init's push refspec),
# stand down so the two pushes don't race each other on the same refs.
ns=$(git config track.namespace 2>/dev/null || true)
[ -n "$ns" ] || ns="refs/meta/branches"
case "$stdin_data" in
*"$ns"*) ;;
*)
  if command -v git-track >/dev/null 2>&1; then
    git-track push --all --quiet --remote "$1" || true
  fi
  ;;
esac
exit 0
`

var scripts = map[string]string{
	"post-checkout": postCheckoutScript,
	"pre-push":      prePushScript,
}

// Dir resolves the active hooks directory, respecting core.hooksPath.
func Dir(g *gitcmd.Runner) (string, error) {
	if p := g.Config("core.hooksPath"); p != "" {
		if !filepath.IsAbs(p) {
			top, err := g.Run("rev-parse", "--show-toplevel")
			if err != nil {
				return "", err
			}
			p = filepath.Join(top, p)
		}
		return p, nil
	}
	gitDir, err := g.Run("rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	return filepath.Join(gitDir, "hooks"), nil
}

// IsOurs reports whether the file at path is a git-track hook script.
func IsOurs(path string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(b), Marker)
}

// Install writes the git-track hooks into dir, chaining any pre-existing
// hooks. Returns the names of hooks that were newly installed or updated.
func Install(dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var installed []string
	for _, name := range Names {
		target := filepath.Join(dir, name)
		chained := target + ".pre-git-track"
		if _, err := os.Stat(target); err == nil && !IsOurs(target) {
			if _, err := os.Stat(chained); err == nil {
				return installed, fmt.Errorf("both %s and %s exist; resolve manually", target, chained)
			}
			if err := os.Rename(target, chained); err != nil {
				return installed, err
			}
		}
		if err := os.WriteFile(target, []byte(scripts[name]), 0o755); err != nil {
			return installed, err
		}
		installed = append(installed, name)
	}
	return installed, nil
}

// Uninstall removes git-track hooks from dir and restores chained originals.
func Uninstall(dir string) error {
	for _, name := range Names {
		target := filepath.Join(dir, name)
		chained := target + ".pre-git-track"
		if IsOurs(target) {
			if err := os.Remove(target); err != nil {
				return err
			}
		}
		if _, err := os.Stat(chained); err == nil {
			if _, err := os.Stat(target); err == nil {
				return fmt.Errorf("cannot restore %s: %s already exists", chained, target)
			}
			if err := os.Rename(chained, target); err != nil {
				return err
			}
		}
	}
	return nil
}
