package cmd

import (
	"fmt"

	"github.com/robertdewilde-dev/git-track/internal/hooks"
	"github.com/spf13/cobra"
)

// Config keys under which init records exactly what it changed, so --undo
// restores the original configuration and nothing more.
const initAddedKey = "track.init.added"

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Configure refspecs and install hooks",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		undo, _ := cmd.Flags().GetBool("undo")
		noHooks, _ := cmd.Flags().GetBool("no-hooks")
		c, err := newCtx()
		if err != nil {
			return err
		}
		if undo {
			return runInitUndo(c)
		}
		return runInit(c, !noHooks)
	},
}

func runInit(c *appCtx, installHooks bool) error {
	remote := c.remote()
	if _, err := c.git.Run("remote", "get-url", remote); err != nil {
		return exitErr(ExitError, "remote %q is not configured; add it first or set `git config track.remote`", remote)
	}
	ns := c.store.Namespace

	fetchKey := fmt.Sprintf("remote.%s.fetch", remote)
	pushKey := fmt.Sprintf("remote.%s.push", remote)
	fetchSpec := fmt.Sprintf("+%s/*:%s/*", ns, ns)
	metaPushSpec := ns + "/*"
	headsPushSpec := "refs/heads/*"

	addValue := func(key, value string) error {
		for _, existing := range c.git.ConfigAll(key) {
			if existing == value {
				return nil // already present, never duplicate
			}
		}
		if _, err := c.git.Run("config", "--add", key, value); err != nil {
			return err
		}
		if _, err := c.git.Run("config", "--add", initAddedKey, key+"="+value); err != nil {
			return err
		}
		info("added %s = %s", key, value)
		return nil
	}

	if err := addValue(fetchKey, fetchSpec); err != nil {
		return err
	}
	// An explicit push refspec overrides push.default, so refs/heads/* must be
	// listed first or normal `git push` breaks. Only needed when we are about
	// to introduce the first push refspec.
	if len(c.git.ConfigAll(pushKey)) == 0 {
		if err := addValue(pushKey, headsPushSpec); err != nil {
			return err
		}
	}
	if err := addValue(pushKey, metaPushSpec); err != nil {
		return err
	}

	if installHooks {
		dir, err := hooks.Dir(c.git)
		if err != nil {
			return err
		}
		installed, err := hooks.Install(dir)
		if err != nil {
			return err
		}
		for _, name := range installed {
			info("installed hook %s", name)
		}
	}
	info("git-track initialized (namespace %s, remote %s); undo with `git track init --undo`", ns, remote)
	if flagJSON {
		return printJSON(map[string]any{"initialized": true, "namespace": ns, "remote": remote})
	}
	return nil
}

func runInitUndo(c *appCtx) error {
	for _, entry := range c.git.ConfigAll(initAddedKey) {
		key, value, ok := cut(entry, "=")
		if !ok {
			continue
		}
		_, _ = c.git.Run("config", "--fixed-value", "--unset", key, value)
		info("removed %s = %s", key, value)
	}
	_, _ = c.git.Run("config", "--remove-section", "track.init")

	dir, err := hooks.Dir(c.git)
	if err == nil {
		if err := hooks.Uninstall(dir); err != nil {
			return exitErr(ExitError, "restoring hooks: %s", err)
		}
	}
	info("git-track configuration removed")
	if flagJSON {
		return printJSON(map[string]any{"initialized": false})
	}
	return nil
}

func cut(s, sep string) (string, string, bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

func init() {
	initCmd.Flags().Bool("undo", false, "remove refspecs and hooks added by init, restoring the original config")
	initCmd.Flags().Bool("no-hooks", false, "configure refspecs only, skip hook installation")
	rootCmd.AddCommand(initCmd)
}
