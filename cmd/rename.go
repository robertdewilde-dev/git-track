package cmd

import (
	"errors"

	"github.com/robertdewilde-dev/git-track/internal/store"
	"github.com/spf13/cobra"
)

var renameCmd = &cobra.Command{
	Use:   "rename <old-branch> <new-branch>",
	Short: "Move metadata after a branch rename (git branch -m does not move it)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		oldBranch, newBranch := args[0], args[1]
		c, err := newCtx()
		if err != nil {
			return err
		}
		sha, err := c.git.Run("rev-parse", "--verify", "--quiet", c.store.Ref(oldBranch))
		if err != nil || sha == "" {
			return store.ErrNoMetadata
		}
		if existing, _ := c.git.Run("rev-parse", "--verify", "--quiet", c.store.Ref(newBranch)); existing != "" {
			return exitErr(ExitError, "metadata already exists for %s", newBranch)
		}
		if _, err := c.git.Run("update-ref", c.store.Ref(newBranch), sha); err != nil {
			return err
		}
		if err := c.store.Delete(oldBranch); err != nil {
			return err
		}
		// Best-effort remote move; the history is unchanged so this is a plain
		// create + delete.
		remote := c.remote()
		if _, err := c.store.Push(remote, newBranch); err != nil && !errors.Is(err, store.ErrNoMetadata) {
			info("warning: could not push %s: %s", c.store.Ref(newBranch), err)
		} else if err := c.store.DeleteRemote(remote, oldBranch); err != nil {
			info("warning: could not delete %s on %s: %s", c.store.Ref(oldBranch), remote, err)
		}
		// The branch's channel moves with it.
		oldCh, newCh := store.BranchChannel(oldBranch), store.BranchChannel(newBranch)
		if tip := c.store.ChannelTip(oldCh); tip != "" && c.store.ChannelTip(newCh) == "" {
			if _, err := c.git.Run("update-ref", c.store.ChannelRef(newCh), tip); err != nil {
				return err
			}
			if err := c.store.SyncChannel(remote, newCh); err != nil {
				info("warning: could not push channel %s: %s", newCh, err)
			} else if err := c.store.DeleteChannel(remote, oldCh); err != nil {
				info("warning: could not delete channel %s: %s", oldCh, err)
			}
		}
		if flagJSON {
			return printJSON(map[string]any{"renamed": true, "from": oldBranch, "to": newBranch})
		}
		info("metadata moved: %s -> %s", oldBranch, newBranch)
		return nil
	},
}

var squashCmd = &cobra.Command{
	Use:   "squash [branch]",
	Short: "Collapse metadata history to a single commit (reclaims object growth)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newCtx()
		if err != nil {
			return err
		}
		branch := flagBranch
		if len(args) == 1 {
			branch = args[0]
		}
		if branch == "" {
			if branch, err = c.branch(); err != nil {
				return err
			}
		}
		doc, oldSHA, err := c.store.Read(branch)
		if err != nil {
			return err
		}
		if err := doc.CheckWritable(); err != nil {
			return err
		}
		ref := c.store.Ref(branch)

		// Refuse when the remote has state we haven't seen — squashing rewrites
		// history, so it must be based on the converged state.
		remote := c.remote()
		remoteSHA, remoteErr := c.store.RemoteSHA(remote, ref)
		if remoteErr == nil && remoteSHA != "" && remoteSHA != oldSHA {
			return &store.ConflictError{Ref: ref, Detail: "remote metadata differs from local"}
		}

		newSHA, err := c.store.Squash(branch, doc, oldSHA, "squash history")
		if err != nil {
			return err
		}
		if remoteErr == nil {
			lease := ref + ":" + remoteSHA
			if remoteSHA == "" {
				lease = ref + ":"
			}
			if _, err := c.git.Run("push", "--quiet", "--no-verify", "--force-with-lease="+lease, remote, newSHA+":"+ref); err != nil {
				return &store.ConflictError{Ref: ref, Detail: "push rejected while squashing"}
			}
		} else {
			info("warning: squash is local-only, could not reach %s", remote)
		}
		if flagJSON {
			return printJSON(map[string]any{"squashed": true, "branch": branch})
		}
		info("%s: metadata history squashed", branch)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(renameCmd, squashCmd)
}
