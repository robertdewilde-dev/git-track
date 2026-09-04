package store

// Read cursors, search, and label usage — the pieces that keep an agent's
// token spend proportional to what changed rather than to what exists.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Cursors record how far a channel has been read. They live in the git dir
// (<git-dir>/track/cursors/<channel>), so they are local to one clone — and,
// because --git-path resolves per worktree, to one worktree: two agents in
// two worktrees of the same clone keep separate cursors. Never synced.
func (s *Store) cursorPath(channel string) (string, error) {
	p, err := s.Git.Run("rev-parse", "--git-path", "track/cursors/"+channel)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) && s.Git.Dir != "" {
		p = filepath.Join(s.Git.Dir, p)
	}
	return p, nil
}

// Cursor returns the SHA the channel was last read up to, or "" when the
// channel was never read here (or the cursor points at a pruned object).
func (s *Store) Cursor(channel string) string {
	p, err := s.cursorPath(channel)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	sha := strings.TrimSpace(string(data))
	if _, err := s.Git.Run("rev-parse", "--verify", "--quiet", sha+"^{commit}"); err != nil {
		return ""
	}
	return sha
}

// SetCursor marks the channel as read up to sha.
func (s *Store) SetCursor(channel, sha string) error {
	p, err := s.cursorPath(channel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(sha+"\n"), 0o644)
}

// MarkRead moves the cursor to the channel's current tip.
func (s *Store) MarkRead(channel string) error {
	tip := s.ChannelTip(channel)
	if tip == "" {
		return nil
	}
	return s.SetCursor(channel, tip)
}

// MessageCount returns how many messages a channel holds locally.
func (s *Store) MessageCount(channel string) int {
	out, err := s.Git.Run("rev-list", "--count", s.ChannelRef(channel), "--")
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(out)
	return n
}

// Unread returns how many messages landed after the cursor (all of them when
// the channel was never read here).
func (s *Store) Unread(channel string) int {
	tip := s.ChannelTip(channel)
	if tip == "" {
		return 0
	}
	cursor := s.Cursor(channel)
	if cursor == "" {
		return s.MessageCount(channel)
	}
	out, err := s.Git.Run("rev-list", "--count", tip, "^"+cursor, "--")
	if err != nil {
		return s.MessageCount(channel)
	}
	n, _ := strconv.Atoi(out)
	return n
}

// ChannelMessage is a message tagged with the channel it came from.
type ChannelMessage struct {
	Channel string `json:"channel"`
	Message
}

// Commit is an ordinary git commit that carries "Label:" trailers.
type Commit struct {
	SHA     string   `json:"sha"`
	Ref     string   `json:"ref"` // the branch (or remote branch) it was found on
	At      string   `json:"at"`
	By      string   `json:"by"`
	Subject string   `json:"subject"`
	Labels  []string `json:"labels,omitempty"`
}

// grepArgs builds git log filters: a case-insensitive literal text match and
// a "Label: <name>" trailer line match (prefilter; callers match exactly).
func grepArgs(text, label string) []string {
	args := []string{"--extended-regexp", "--regexp-ignore-case"}
	if text != "" {
		args = append(args, "--grep="+regexp.QuoteMeta(text))
	}
	if label != "" {
		args = append(args, "--grep=^Label: "+regexp.QuoteMeta(label)+"$")
	}
	if text != "" && label != "" {
		args = append(args, "--all-match")
	}
	return args
}

// SearchMessages finds messages across every channel whose text contains
// text (case-insensitive) and/or which carry label, newest first, in one git
// log. Either filter may be empty, not both. limit <= 0 means all.
func (s *Store) SearchMessages(text, label string, limit int) ([]ChannelMessage, error) {
	if text == "" && label == "" {
		return nil, fmt.Errorf("search needs text or a label")
	}
	channels, err := s.Channels()
	if err != nil || len(channels) == 0 {
		return nil, err
	}
	ns := s.ChannelsNamespace() + "/"
	args := []string{"log", "--format=%S%x00%H%x00%aI%x00%an%x00%B%x1e"}
	args = append(args, grepArgs(text, label)...)
	for _, ch := range channels {
		args = append(args, ns+ch)
	}
	out, err := s.Git.Run(append(args, "--")...)
	if err != nil {
		return nil, err
	}
	var found []ChannelMessage
	for _, rec := range strings.Split(out, "\x1e") {
		parts := strings.SplitN(strings.TrimLeft(rec, "\n"), "\x00", 5)
		if len(parts) != 5 {
			continue
		}
		body, by, labels := parseMessage(parts[4], parts[3])
		if label != "" && !contains(labels, label) {
			continue
		}
		found = append(found, ChannelMessage{
			Channel: strings.TrimPrefix(parts[0], ns),
			Message: Message{SHA: parts[1], At: parts[2], By: by, Labels: labels, Body: body},
		})
		if limit > 0 && len(found) >= limit {
			break
		}
	}
	return found, nil
}

// CommitsWithLabel finds ordinary commits on local and remote-tracking
// branches whose message carries a "Label: <label>" trailer, newest first.
// git-track never writes such commits; people add the trailer themselves
// (git commit --trailer "Label: bug") to tie a commit into the vocabulary.
func (s *Store) CommitsWithLabel(label string, limit int) ([]Commit, error) {
	args := []string{"log", "--branches", "--remotes",
		"--format=%S%x00%H%x00%aI%x00%an%x00%s%x00%(trailers:key=Label,valueonly,separator=%x1f)%x1e"}
	args = append(args, grepArgs("", label)...)
	if limit > 0 {
		args = append(args, fmt.Sprintf("-n%d", limit))
	}
	out, err := s.Git.Run(append(args, "--")...)
	if err != nil {
		return nil, err
	}
	var found []Commit
	for _, rec := range strings.Split(out, "\x1e") {
		parts := strings.SplitN(strings.TrimLeft(rec, "\n"), "\x00", 6)
		if len(parts) != 6 {
			continue
		}
		labels := splitLabels(parts[5])
		if !contains(labels, label) {
			continue
		}
		found = append(found, Commit{SHA: parts[1], Ref: parts[0], At: parts[2], By: parts[3], Subject: parts[4], Labels: labels})
	}
	return found, nil
}

// LabelUsage counts, per label, how many chat messages and how many ordinary
// commits carry it (branch metadata is counted by the caller, which already
// reads the documents). Two git log passes, no per-label work.
func (s *Store) LabelUsage() (messages, commits map[string]int) {
	messages, commits = map[string]int{}, map[string]int{}
	count := func(into map[string]int, args ...string) {
		out, err := s.Git.Run(append(args, "--")...)
		if err != nil {
			return
		}
		for _, line := range strings.Split(out, "\n") {
			for _, l := range splitLabels(line) {
				into[l]++
			}
		}
	}
	if channels, _ := s.Channels(); len(channels) > 0 {
		args := []string{"log", "--format=%(trailers:key=Label,valueonly,separator=%x1f)"}
		for _, ch := range channels {
			args = append(args, s.ChannelRef(ch))
		}
		count(messages, args...)
	}
	count(commits, "log", "--branches", "--remotes", "--extended-regexp", "--grep=^Label: ",
		"--format=%(trailers:key=Label,valueonly,separator=%x1f)")
	return messages, commits
}

func splitLabels(raw string) []string {
	var labels []string
	for _, l := range strings.Split(raw, "\x1f") {
		if l = strings.TrimSpace(l); l != "" {
			labels = append(labels, l)
		}
	}
	return labels
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
