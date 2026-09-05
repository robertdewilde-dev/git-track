package cmd

// Events: every channel message is an event; chat is the type "chat". emit
// posts a typed event with an optional JSON payload, and `watch --exec` runs
// a local command per event with a CloudEvents 1.0 envelope on stdin — the
// smallest possible trigger layer. Triggers are deliberately local (a flag
// on the machine that watches), never shared config: nothing pushed to a
// repository may decide what runs on your machine.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/robertdewilde-dev/git-track/internal/store"
	"github.com/spf13/cobra"
)

// envelope renders a message as a CloudEvents 1.0 JSON object. Core
// attributes follow the spec; git-track specifics ride along as extension
// attributes (channel, by, body, labels).
func envelope(channel string, m store.Message) map[string]any {
	ev := map[string]any{
		"specversion": "1.0",
		"id":          m.SHA,
		"source":      "git-track://" + m.By,
		"type":        m.Type,
		"time":        m.At,
		"channel":     channel,
		"by":          m.By,
		"body":        m.Body,
	}
	if m.Subject != "" {
		ev["subject"] = m.Subject
	}
	if len(m.Labels) > 0 {
		ev["labels"] = strings.Join(m.Labels, ",")
	}
	if len(m.Data) > 0 {
		ev["datacontenttype"] = "application/json"
		ev["data"] = m.Data
	}
	return ev
}

// runHandler executes command through the shell with the event envelope on
// stdin and TRACK_* variables in the environment. Handlers run one at a
// time, in event order; a failing handler is reported, never fatal.
func runHandler(command, channel string, m store.Message) error {
	shell, flag := "sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/C"
	}
	cmd := exec.Command(shell, flag, command)
	payload, _ := json.Marshal(envelope(channel, m))
	cmd.Stdin = strings.NewReader(string(payload) + "\n")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(),
		"TRACK_CHANNEL="+channel,
		"TRACK_TYPE="+m.Type,
		"TRACK_SHA="+m.SHA,
		"TRACK_BY="+m.By,
		"TRACK_SUBJECT="+m.Subject,
		"TRACK_LABELS="+strings.Join(m.Labels, ","),
		"TRACK_BODY="+m.Body,
		"TRACK_DATA="+string(m.Data),
	)
	return cmd.Run()
}

// readData resolves --data: inline JSON, @file, or - for stdin.
func readData(raw string) (json.RawMessage, error) {
	if raw == "" {
		return nil, nil
	}
	var data []byte
	switch {
	case raw == "-":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, err
		}
		data = b
	case strings.HasPrefix(raw, "@"):
		b, err := os.ReadFile(raw[1:])
		if err != nil {
			return nil, err
		}
		data = b
	default:
		data = []byte(raw)
	}
	if !json.Valid(data) {
		return nil, exitErr(ExitError, "--data is not valid JSON")
	}
	return json.RawMessage(data), nil
}

var emitCmd = &cobra.Command{
	Use:   "emit <type> [message]",
	Short: "Post a typed event (with optional JSON data) to a channel",
	Long: `Post an event to a channel. Events are what 'say' posts too — a chat message
is the event type "chat" — so everything lands in the same log, syncs the
same way, and is read with 'chat', 'watch', and 'search'.

Types are dotted names, lowercase (tests.failed, deploy.done, review.request).
--data attaches a JSON payload (inline, @file, or - for stdin); --subject
names what the event is about (a branch, a file, an issue). Every consumer
sees the event as a CloudEvents 1.0 envelope: 'watch --exec' hands it to
your handler on stdin. Any tool can publish with plain git; see SPEC.md.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		channel, _ := cmd.Flags().GetString("channel")
		labels, _ := cmd.Flags().GetStringArray("label")
		subject, _ := cmd.Flags().GetString("subject")
		rawData, _ := cmd.Flags().GetString("data")
		c, err := newCtx()
		if err != nil {
			return jsonError(err)
		}
		typ := strings.TrimSpace(args[0])
		if typ == "" || strings.ContainsAny(typ, " \t\n") {
			return jsonError(exitErr(ExitError, "invalid event type %q", args[0]))
		}
		data, err := readData(rawData)
		if err != nil {
			return jsonError(err)
		}
		body := ""
		if len(args) == 2 {
			body = args[1]
		}
		m := store.Message{Type: typ, Subject: subject, Labels: labels, Body: body, Data: data}
		channel, sha, synced, err := publish(c, channel, m)
		if err != nil {
			return jsonError(err)
		}
		if flagJSON {
			return printJSON(map[string]any{"channel": channel, "sha": sha, "type": typ, "synced": synced})
		}
		info("#%s: %s emitted", channel, typ)
		return nil
	},
}

func init() {
	emitCmd.Flags().StringP("channel", "c", "", "channel to post to (default: current branch)")
	emitCmd.Flags().StringArray("label", nil, "label the event (repeatable)")
	emitCmd.Flags().String("subject", "", "what the event is about (branch, file, issue)")
	emitCmd.Flags().String("data", "", "JSON payload: inline, @file, or - for stdin")
	rootCmd.AddCommand(emitCmd)
}

// formatMessage renders one message for humans: a [type] marker for
// non-chat events, labels, and compact data.
func formatMessage(m store.Message, ts string) string {
	head := dim(ts) + "  " + bold(m.By)
	if m.Type != store.ChatType {
		head += "  " + bold("["+m.Type+"]")
	}
	if m.Subject != "" {
		head += "  " + m.Subject
	}
	if len(m.Labels) > 0 {
		head += "  [" + strings.Join(m.Labels, ", ") + "]"
	}
	out := head + "\n" + m.Body + "\n"
	if len(m.Data) > 0 {
		out += dim(fmt.Sprintf("data: %s", m.Data)) + "\n"
	}
	return out
}
