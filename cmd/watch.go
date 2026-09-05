package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/robertdewilde-dev/git-track/internal/store"
	"github.com/spf13/cobra"
)

// watchOptions configures one watch loop.
type watchOptions struct {
	channels []string      // explicit channel names
	all      bool          // every channel on the remote, including ones created later
	interval time.Duration // poll interval
	timeout  time.Duration // give up after this long (0 = never)
	once     bool          // stop after the first batch of new messages
	types    []string      // only emit events of these types (empty = all)
}

// watchChannels polls the remote (one ls-remote per tick, no object
// transfer until a tip changes) and calls emit for every message that lands
// after the watch started, oldest first. The first tick establishes the
// baseline: whatever the remote holds then is "seen". It also notices local
// changes (a post from another worktree of this clone) and syncs unsent local
// messages it comes across. Returns the number of messages emitted; the first
// tick's remote error is fatal, later ones are retried with backoff.
func watchChannels(c *appCtx, opts watchOptions, emit func(channel string, m store.Message)) (int, error) {
	remote := c.remote()
	lastSeen := map[string]string{}
	for _, name := range opts.channels {
		lastSeen[name] = c.store.ChannelTip(name)
	}
	var deadline time.Time
	if opts.timeout > 0 {
		deadline = time.Now().Add(opts.timeout)
	}
	emitted, failures := 0, 0
	for tick := 0; ; tick++ {
		remoteTips, err := c.store.RemoteChannels(remote)
		if err != nil {
			if tick == 0 {
				return 0, err
			}
			failures++
			if failures == 1 {
				info("watch: %s (retrying)", err)
			}
			wait := opts.interval * time.Duration(1<<min(failures, 4))
			if wait > 30*time.Second {
				wait = 30 * time.Second
			}
			time.Sleep(wait)
			continue
		}
		failures = 0
		if opts.all {
			for name := range remoteTips {
				if _, known := lastSeen[name]; !known {
					lastSeen[name] = "" // new channel: every message is new
				}
			}
		}
		names := make([]string, 0, len(lastSeen))
		for name := range lastSeen {
			names = append(names, name)
		}
		sort.Strings(names)
		batch := 0
		for _, name := range names {
			var msgs []store.Message
			tip := c.store.ChannelTip(name)
			if rtip := remoteTips[name]; rtip != "" && rtip != tip {
				var err error
				if msgs, tip, err = c.store.PullChannel(remote, name, rtip, lastSeen[name]); err != nil {
					info("watch: %s: %s", name, err)
					continue
				}
			} else if tip != lastSeen[name] && tip != "" {
				msgs, _ = c.store.MessagesSince(name, lastSeen[name], 0)
			}
			if tick == 0 {
				msgs = nil // the first tick is the baseline: only what lands after this is new
			}
			for i := len(msgs) - 1; i >= 0; i-- {
				if len(opts.types) > 0 && !slices.Contains(opts.types, msgs[i].Type) {
					continue
				}
				emit(name, msgs[i])
				batch++
			}
			if len(msgs) > 0 {
				_ = c.store.SetCursor(name, tip)
			}
			lastSeen[name] = tip
		}
		emitted += batch
		if opts.once && batch > 0 {
			return emitted, nil
		}
		if !deadline.IsZero() {
			left := time.Until(deadline)
			if left <= 0 {
				return emitted, nil
			}
			if left < opts.interval {
				time.Sleep(left)
				continue
			}
		}
		time.Sleep(opts.interval)
	}
}

var watchCmd = &cobra.Command{
	Use:   "watch [channel...]",
	Short: "Stream new channel messages as they arrive (polls the remote)",
	Long: `Watch channels and print each new message as it lands — 'tail -f' for
chat. The remote is polled with one cheap ls-remote per interval; objects are
fetched only when a channel actually changed. Unsent local messages on a
watched channel are synced along the way.

By default this watches the current branch's channel and "main". Name
channels explicitly, or use --all to watch every channel on the remote
(including ones created later).

With --once the command exits after the first batch of new messages (exit 0);
with --timeout it gives up after that long (exit 2 if nothing arrived). Use
both to block until someone replies. With --json every message is one JSON
object per line (JSON Lines), suitable for piping.

Triggers: --type keeps only events of the given types (repeatable), and
--exec runs a shell command per event — the CloudEvents envelope on stdin,
TRACK_CHANNEL/TRACK_TYPE/TRACK_SHA/TRACK_BY/TRACK_SUBJECT/TRACK_LABELS/
TRACK_BODY/TRACK_DATA in the environment. Handlers run one at a time in
event order; a failing handler is reported, not fatal. Triggers are local by
design: nothing pushed to the repository can decide what runs here.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		all, _ := cmd.Flags().GetBool("all")
		interval, _ := cmd.Flags().GetDuration("interval")
		timeout, _ := cmd.Flags().GetDuration("timeout")
		once, _ := cmd.Flags().GetBool("once")
		tail, _ := cmd.Flags().GetInt("tail")
		types, _ := cmd.Flags().GetStringArray("type")
		handler, _ := cmd.Flags().GetString("exec")
		c, err := newCtx()
		if err != nil {
			return jsonError(err)
		}
		if interval < 200*time.Millisecond {
			return jsonError(exitErr(ExitError, "--interval must be at least 200ms"))
		}
		channels := args
		if len(channels) == 0 && !all {
			channels = []string{store.MainChannel}
			if branch, err := c.branch(); err == nil {
				channels = append(channels, store.BranchChannel(branch))
			}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		emit := func(channel string, m store.Message) {
			if handler != "" {
				if err := runHandler(handler, channel, m); err != nil {
					info("watch: handler failed for %s %s: %s", m.Type, m.SHA[:12], err)
				}
			}
			if flagJSON {
				_ = enc.Encode(store.ChannelMessage{Channel: channel, Message: m})
				return
			}
			ts := m.At
			if t, err := time.Parse(time.RFC3339, m.At); err == nil {
				ts = t.Local().Format("15:04:05")
			}
			fmt.Printf("%s %s\n", bold("#"+channel), formatMessage(m, ts))
		}
		if tail > 0 {
			for _, name := range channels {
				if msgs, err := c.store.Messages(name, tail); err == nil {
					for i := len(msgs) - 1; i >= 0; i-- {
						emit(name, msgs[i])
					}
				}
			}
		}
		if !flagJSON {
			info("watching %s every %s (ctrl-c to stop)", strings.Join(channelsLabel(channels, all), ", "), interval)
		}
		n, err := watchChannels(c, watchOptions{channels: channels, all: all, interval: interval, timeout: timeout, once: once, types: types}, emit)
		if err != nil {
			return jsonError(err)
		}
		if n == 0 {
			return exitErr(ExitNoMetadata, "no new messages within %s", timeout)
		}
		return nil
	},
}

func channelsLabel(channels []string, all bool) []string {
	if all {
		return []string{"all channels"}
	}
	out := make([]string, len(channels))
	for i, ch := range channels {
		out[i] = "#" + ch
	}
	return out
}

func init() {
	watchCmd.Flags().BoolP("all", "a", false, "watch every channel on the remote")
	watchCmd.Flags().Duration("interval", 2*time.Second, "poll interval")
	watchCmd.Flags().Duration("timeout", 0, "stop after this long (0 = never)")
	watchCmd.Flags().Bool("once", false, "exit after the first batch of new messages")
	watchCmd.Flags().Int("tail", 0, "print the last N existing messages first")
	watchCmd.Flags().StringArray("type", nil, "only events of this type (repeatable)")
	watchCmd.Flags().String("exec", "", "run this shell command per event (envelope on stdin)")
	rootCmd.AddCommand(watchCmd)
}
