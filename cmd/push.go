package cmd

import (
	"errors"

	"github.com/robertdewilde-dev/git-track/internal/store"
	"github.com/spf13/cobra"
)

var pushCmd = &cobra.Command{
	Use:   "push [branch]",
	Short: "Push metadata refs to the remote",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		all, _ := cmd.Flags().GetBool("all")
		remote, _ := cmd.Flags().GetString("remote")
		c, err := newCtx()
		if err != nil {
			return err
		}
		if remote == "" {
			remote = c.remote()
		}

		var branches []string
		switch {
		case all:
			if branches, err = c.store.Branches(); err != nil {
				return err
			}
		case len(args) == 1:
			branches = []string{args[0]}
		default:
			b, err := c.branch()
			if err != nil {
				return err
			}
			branches = []string{b}
		}

		var results []store.PushResult
		for _, b := range branches {
			res, err := c.store.Push(remote, b)
			if err != nil {
				if all && errors.Is(err, store.ErrNoMetadata) {
					continue
				}
				return err
			}
			results = append(results, res)
			if res.Status != "up-to-date" {
				info("%s: %s", b, res.Status)
			}
		}
		if all {
			// --all also syncs the channels and shared definitions (they are
			// not in the configured refspecs; the tool is their sync path).
			channels, _ := c.store.Channels()
			for _, ch := range channels {
				if err := c.store.SyncChannel(remote, ch); err != nil {
					info("channel %s: sync failed (%s)", ch, err)
				}
			}
			if _, _, err := c.store.ReadDefs(); err == nil {
				if res, err := c.store.PushRef(remote, c.store.DefsRef()); err != nil {
					info("definitions: sync failed (%s)", err)
				} else if res.Status != "up-to-date" {
					info("definitions: %s", res.Status)
				}
			}
		}
		if flagJSON {
			if results == nil {
				results = []store.PushResult{}
			}
			return printJSON(results)
		}
		return nil
	},
}

var fetchCmd = &cobra.Command{
	Use:   "fetch",
	Short: "Fetch metadata refs from the remote",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		remote, _ := cmd.Flags().GetString("remote")
		c, err := newCtx()
		if err != nil {
			return err
		}
		if remote == "" {
			remote = c.remote()
		}
		if err := c.store.Fetch(remote); err != nil {
			return err
		}
		if err := c.store.FetchDefs(remote); err != nil {
			info("definitions: %s", err)
		}
		behind, err := c.store.FetchChannels(remote)
		if err != nil {
			info("channels: %s", err)
		}
		for _, ch := range behind {
			info("channel %s has unsent local messages; run `git track push --all` (or `git track say`) to merge", ch)
		}
		if flagJSON {
			return printJSON(map[string]any{"fetched": true, "remote": remote, "channelsBehind": behind})
		}
		info("metadata refs fetched from %s", remote)
		return nil
	},
}

func init() {
	pushCmd.Flags().Bool("all", false, "push metadata for all branches")
	pushCmd.Flags().String("remote", "", "remote to push to (default: track.remote config or origin)")
	fetchCmd.Flags().String("remote", "", "remote to fetch from (default: track.remote config or origin)")
	rootCmd.AddCommand(pushCmd, fetchCmd)
}
