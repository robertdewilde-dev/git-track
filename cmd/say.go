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

Without --channel the message goes to the channel named after the current
branch. Pass '-' to read the message body from stdin.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		channel, _ := cmd.Flags().GetString("channel")
		labels, _ := cmd.Flags().GetStringArray("label")
		c, err := newCtx()
		if err != nil {
			return jsonError(err)
		}
		if channel == "" {
			if channel, err = c.branch(); err != nil {
				return jsonError(err)
			}
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
		sha, err := c.store.AppendMessage(channel, body, lock.Actor(), labels)
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

var chatCmd = &cobra.Command{
	Use:   "chat [channel]",
	Short: "Read a channel's messages (default: this branch's channel)",
	Long: `Read the messages in a channel, oldest first. Messages arrive with
'git track fetch' (channels are pull-based; poll to see new messages).
Filter with --label; --limit bounds how many recent messages are shown.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		limit, _ := cmd.Flags().GetInt("limit")
		labelFilter, _ := cmd.Flags().GetString("label")
		c, err := newCtx()
		if err != nil {
			return jsonError(err)
		}
		channel := ""
		if len(args) == 1 {
			channel = args[0]
		} else if channel, err = c.branch(); err != nil {
			return jsonError(err)
		}
		// Over-fetch when filtering so --limit counts matching messages.
		fetchLimit := limit
		if labelFilter != "" {
			fetchLimit = 0
		}
		msgs, err := c.store.Messages(channel, fetchLimit)
		if err != nil {
			if errors.Is(err, store.ErrNoMetadata) {
				err = exitErr(ExitNoMetadata, "no messages in channel %q", channel)
			}
			return jsonError(err)
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
	rootCmd.AddCommand(sayCmd, chatCmd)
}
