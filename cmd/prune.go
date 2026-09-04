package cmd

import (
	"strings"

	"github.com/robertdewilde-dev/git-track/internal/store"
	"github.com/spf13/cobra"
)

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Delete metadata for branches that no longer exist",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		alsoRemote, _ := cmd.Flags().GetBool("remote")
		c, err := newCtx()
		if err != nil {
			return err
		}
		remote := c.remote()
		branches, err := c.store.Branches()
		if err != nil {
			return err
		}
		var pruned []string
		for _, b := range branches {
			if refExists(c, "refs/heads/"+b) || refExists(c, "refs/remotes/"+remote+"/"+b) {
				continue
			}
			if !dryRun {
				if err := c.store.Delete(b); err != nil {
					return err
				}
				if alsoRemote {
					if err := c.store.DeleteRemote(remote, b); err != nil {
						info("warning: could not delete %s on %s: %s", c.store.Ref(b), remote, err)
					}
				}
			}
			pruned = append(pruned, b)
			info("pruned metadata for %s", b)
		}
		// Branch channels follow the same rule: gone branch, gone channel.
		// Named channels (main, planning, ...) are never pruned.
		channels, _ := c.store.Channels()
		var prunedChannels []string
		for _, ch := range channels {
			b, isBranch := strings.CutPrefix(ch, store.BranchChannel(""))
			if !isBranch || refExists(c, "refs/heads/"+b) || refExists(c, "refs/remotes/"+remote+"/"+b) {
				continue
			}
			if !dryRun {
				if alsoRemote {
					if err := c.store.DeleteChannel(remote, ch); err != nil {
						info("warning: could not delete channel %s: %s", ch, err)
					}
				} else if _, err := c.git.Run("update-ref", "-d", c.store.ChannelRef(ch)); err != nil {
					return err
				}
			}
			prunedChannels = append(prunedChannels, ch)
			info("pruned channel %s", ch)
		}
		if flagJSON {
			if pruned == nil {
				pruned = []string{}
			}
			if prunedChannels == nil {
				prunedChannels = []string{}
			}
			return printJSON(map[string]any{"pruned": pruned, "prunedChannels": prunedChannels, "dryRun": dryRun})
		}
		if len(pruned)+len(prunedChannels) == 0 {
			info("nothing to prune")
		}
		return nil
	},
}

func refExists(c *appCtx, ref string) bool {
	_, err := c.git.Run("show-ref", "--verify", "--quiet", ref)
	return err == nil
}

func init() {
	pruneCmd.Flags().Bool("dry-run", false, "report what would be pruned without deleting")
	pruneCmd.Flags().Bool("remote", false, "also delete the pruned refs on the remote")
	rootCmd.AddCommand(pruneCmd)
}
