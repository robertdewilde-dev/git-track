package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/robertdewilde-dev/git-track/internal/lock"
	"github.com/robertdewilde-dev/git-track/internal/store"
	"github.com/spf13/cobra"
)

var sayCmd = &cobra.Command{
	Use:   "say <message | ->",
	Short: "Post a message to a channel (default: this branch's channel)",
	Long: `Post a message to an async channel. Each message is one commit under
refs/meta/channels/<name>; the message syncs to the remote immediately, and a
concurrent post from another machine is merged automatically (messages are
replayed onto the remote tip, never lost). Other agents read with
'git track chat' after 'git track fetch'.

Without --channel the message goes to the current branch's channel
(branches/<branch>). Use '-c main' for the shared coordination channel that
every repository has — questions, fan-out, announcements — or any named
channel. Pass '-' to read the message body from stdin.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		channel, _ := cmd.Flags().GetString("channel")
		labels, _ := cmd.Flags().GetStringArray("label")
		c, err := newCtx()
		if err != nil {
			return jsonError(err)
		}
		if channel == "" {
			branch, err := c.branch()
			if err != nil {
				return jsonError(err)
			}
			channel = store.BranchChannel(branch)
		}
		body := args[0]
		if body == "-" {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return jsonError(err)
			}
			body = string(data)
		}
		if strings.TrimSpace(body) == "" {
			return jsonError(exitErr(ExitError, "empty message"))
		}
		hintUndefinedLabels(c, labels)
		sha, err := postMessage(c, channel, body, labels)
		if err != nil {
			return jsonError(err)
		}
		synced := true
		if err := c.store.SyncChannel(c.remote(), channel); err != nil {
			synced = false
			info("message saved locally; sync failed (%s) — it will sync on the next say or `git track push --all`", err)
		}
		if flagJSON {
			return printJSON(map[string]any{"channel": channel, "sha": sha, "synced": synced})
		}
		info("#%s: message posted", channel)
		return nil
	},
}

// postMessage appends a message and keeps the read cursor in step: when the
// poster had read everything before posting, their own message is not
// "unread" afterwards.
func postMessage(c *appCtx, channel, body string, labels []string) (string, error) {
	prev := c.store.ChannelTip(channel)
	sha, err := c.store.AppendMessage(channel, body, lock.Actor(), labels)
	if err != nil {
		return "", err
	}
	if c.store.Cursor(channel) == prev {
		_ = c.store.SetCursor(channel, sha)
	}
	return sha, nil
}

var chatCmd = &cobra.Command{
	Use:   "chat [channel]",
	Short: "Read a channel's messages (default: this branch's channel)",
	Long: `Read the messages in a channel, oldest first. Messages arrive with
'git track fetch'; to react to new messages as they arrive use
'git track watch'. Filter with --label; --limit bounds how many recent
messages are shown; --since <sha> shows only messages after that one.

--unread shows only what landed since this channel was last read here, then
marks it read. Every unfiltered read marks the channel read; the cursor is
per clone and per worktree (stored in the git dir, never synced), so
'git track overview' can tell you what is new for you.

Channels: the current branch's channel by default, "main" for the shared
coordination channel, or any named channel.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		limit, _ := cmd.Flags().GetInt("limit")
		labelFilter, _ := cmd.Flags().GetString("label")
		since, _ := cmd.Flags().GetString("since")
		unread, _ := cmd.Flags().GetBool("unread")
		c, err := newCtx()
		if err != nil {
			return jsonError(err)
		}
		channel := ""
		if len(args) == 1 {
			channel = args[0]
		} else {
			branch, err := c.branch()
			if err != nil {
				return jsonError(err)
			}
			channel = store.BranchChannel(branch)
		}
		if unread {
			since = c.store.Cursor(channel)
			limit = 0
		}
		// Over-fetch when filtering so --limit counts matching messages.
		fetchLimit := limit
		if labelFilter != "" {
			fetchLimit = 0
		}
		msgs, err := c.store.MessagesSince(channel, since, fetchLimit)
		if err != nil {
			if errors.Is(err, store.ErrNoMetadata) {
				err = exitErr(ExitNoMetadata, "no messages in channel %q", channel)
			}
			return jsonError(err)
		}
		if labelFilter == "" {
			_ = c.store.MarkRead(channel)
		}
		if labelFilter != "" {
			msgs = slices.DeleteFunc(msgs, func(m store.Message) bool {
				return !slices.Contains(m.Labels, labelFilter)
			})
			if limit > 0 && len(msgs) > limit {
				msgs = msgs[:limit]
			}
		}
		if flagJSON {
			if msgs == nil {
				msgs = []store.Message{}
			}
			return printJSON(map[string]any{"channel": channel, "messages": msgs})
		}
		if unread && len(msgs) == 0 {
			info("#%s: no unread messages", channel)
			return nil
		}
		fmt.Printf("%s\n", bold("#"+channel))
		for i := len(msgs) - 1; i >= 0; i-- { // oldest first
			m := msgs[i]
			ts := m.At
			if t, err := time.Parse(time.RFC3339, m.At); err == nil {
				ts = t.Local().Format("2006-01-02 15:04")
			}
			labels := ""
			if len(m.Labels) > 0 {
				labels = "  [" + strings.Join(m.Labels, ", ") + "]"
			}
			fmt.Printf("\n%s  %s%s\n%s\n", dim(ts), bold(m.By), labels, m.Body)
		}
		return nil
	},
}

func init() {
	sayCmd.Flags().StringP("channel", "c", "", "channel to post to (default: current branch)")
	sayCmd.Flags().StringArray("label", nil, "label the message (repeatable)")
	chatCmd.Flags().IntP("limit", "n", 30, "show at most this many recent messages (0 = all)")
	chatCmd.Flags().String("label", "", "only show messages carrying this label")
	chatCmd.Flags().String("since", "", "only show messages after this message sha")
	chatCmd.Flags().Bool("unread", false, "only messages since this channel was last read here")
	rootCmd.AddCommand(sayCmd, chatCmd)
}
