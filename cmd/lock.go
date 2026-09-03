package cmd

import (
	"errors"
	"time"

	"github.com/robertdewilde-dev/git-track/internal/lock"
	"github.com/robertdewilde-dev/git-track/internal/schema"
	"github.com/robertdewilde-dev/git-track/internal/store"
	"github.com/spf13/cobra"
)

// acquireLock implements the locking protocol: local pre-check, CAS write,
// then the force-with-lease push that is the real mutex. Shared by the lock
// command and the MCP server. Returns the lock value on success.
func acquireLock(c *appCtx, branch string, ttl time.Duration, force bool) (string, error) {
	if doc, _, err := c.store.Read(branch); err == nil {
		if li := lock.Active(doc, time.Now()); li != nil && !lock.SameActor(li.Owner) && !force {
			return "", &lockHeldError{owner: li.Owner}
		}
	}
	prevSHA, _ := c.git.Run("rev-parse", "--verify", "--quiet", c.store.Ref(branch))
	value := lock.Value()
	_, err := c.mutateDoc(branch, force, "lock by "+value, func(d schema.Doc) error {
		if err := d.Set("agent.lockedBy", value); err != nil {
			return err
		}
		if err := d.Set("agent.lockedAt", time.Now().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
		if ttl > 0 {
			return d.Set("agent.lockTtl", ttl.String())
		}
		d.Unset("agent.lockTtl")
		return nil
	})
	if err != nil {
		return "", err
	}
	// The push is what makes the lock real: force-with-lease acts as the
	// compare-and-swap. A rejection means another actor won the race.
	if _, err := c.store.Push(c.remote(), branch); err != nil {
		var conflict *store.ConflictError
		if errors.As(err, &conflict) {
			rollbackRef(c, branch, prevSHA)
			return "", exitErr(ExitLocked, "lock race lost: remote metadata changed (run `git track fetch` to see the holder)")
		}
		info("warning: lock is local-only, could not push: %s", err)
	}
	return value, nil
}

// releaseLock clears the lock fields and pushes. Only the owning actor (or
// --force, or an expired lock) may release.
func releaseLock(c *appCtx, branch string, force bool) error {
	doc, _, err := c.store.Read(branch)
	if err != nil {
		return err
	}
	li := lock.FromDoc(doc)
	if li == nil {
		return nil
	}
	if !lock.SameActor(li.Owner) && !li.Expired(time.Now()) && !force {
		return &lockHeldError{owner: li.Owner}
	}
	_, err = c.mutateDoc(branch, true, "unlock", func(d schema.Doc) error {
		if err := d.Set("agent.lockedBy", nil); err != nil {
			return err
		}
		d.Unset("agent.lockedAt")
		d.Unset("agent.lockTtl")
		return nil
	})
	if err != nil {
		return err
	}
	if _, err := c.store.Push(c.remote(), branch); err != nil {
		var conflict *store.ConflictError
		if errors.As(err, &conflict) {
			return err
		}
		info("warning: unlock is local-only, could not push: %s", err)
	}
	return nil
}

var lockCmd = &cobra.Command{
	Use:   "lock",
	Short: "Acquire the agent lock for this branch (distributed mutex)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		force, _ := cmd.Flags().GetBool("force")
		ttl, _ := cmd.Flags().GetString("ttl")
		var ttlDur time.Duration
		if ttl != "" {
			var err error
			if ttlDur, err = time.ParseDuration(ttl); err != nil || ttlDur <= 0 {
				return exitErr(ExitError, "invalid --ttl %q (use Go durations like 30m, 2h)", ttl)
			}
		}
		c, err := newCtx()
		if err != nil {
			return err
		}
		branch, err := c.branch()
		if err != nil {
			return err
		}
		value, err := acquireLock(c, branch, ttlDur, force)
		if err != nil {
			return err
		}
		if flagJSON {
			return printJSON(map[string]any{"locked": true, "lockedBy": value, "branch": branch})
		}
		info("%s: locked by %s", branch, value)
		return nil
	},
}

var unlockCmd = &cobra.Command{
	Use:   "unlock",
	Short: "Release the agent lock for this branch",
	Args:  cobra.NoArgs,
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
		if err := releaseLock(c, branch, force); err != nil {
			return err
		}
		if flagJSON {
			return printJSON(map[string]any{"locked": false, "branch": branch})
		}
		info("%s: unlocked", branch)
		return nil
	},
}

// rollbackRef restores the metadata ref after a lost lock race so local state
// does not diverge from the remote.
func rollbackRef(c *appCtx, branch, prevSHA string) {
	ref := c.store.Ref(branch)
	if prevSHA == "" {
		_, _ = c.git.Run("update-ref", "-d", ref)
	} else {
		_, _ = c.git.Run("update-ref", ref, prevSHA)
	}
}

func init() {
	lockCmd.Flags().Bool("force", false, "steal a lock held by another actor")
	lockCmd.Flags().String("ttl", "", "auto-expire the lock after this duration (e.g. 30m)")
	unlockCmd.Flags().Bool("force", false, "release a lock held by another actor")
	rootCmd.AddCommand(lockCmd, unlockCmd)
}
