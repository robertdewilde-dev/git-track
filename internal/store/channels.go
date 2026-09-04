package store

// Channels: async message streams stored as commit chains, one commit per
// message. The commit message carries the body plus "Label:" / "By:" trailers,
// so `git log <channel-ref>` is the chat log. Channel and label definitions
// live in a shared defs document at DefsRef.

import (
	"fmt"
	"strings"

	"github.com/robertdewilde-dev/git-track/internal/schema"
)

// DefsFile is the file the defs commit's tree contains.
const DefsFile = "defs.json"

// Message is one chat message.
type Message struct {
	SHA    string   `json:"sha"`
	At     string   `json:"at"`
	By     string   `json:"by"`
	Labels []string `json:"labels,omitempty"`
	Body   string   `json:"body"`
}

// metaParent returns the parent path of the branch namespace
// ("refs/meta/branches" -> "refs/meta").
func (s *Store) metaParent() string {
	if i := strings.LastIndexByte(s.Namespace, '/'); i > 0 {
		return s.Namespace[:i]
	}
	return s.Namespace
}

// ChannelsNamespace is the ref prefix for channels (default refs/meta/channels).
func (s *Store) ChannelsNamespace() string {
	return s.metaParent() + "/channels"
}

// ChannelRef returns the ref for one channel.
func (s *Store) ChannelRef(name string) string {
	return s.ChannelsNamespace() + "/" + name
}

// MainChannel is the well-known coordination channel every repository has:
// questions, fan-out, announcements — anything not tied to one branch.
const MainChannel = "main"

// BranchChannel is the name of a branch's own channel. Branch channels live
// under "branches/" so they can never collide with named channels.
func BranchChannel(branch string) string {
	return "branches/" + branch
}

// DefsRef is the ref holding label and channel definitions
// (default refs/meta/defs/all; a subdirectory so wildcard refspecs apply).
func (s *Store) DefsRef() string {
	return s.metaParent() + "/defs/all"
}

// Channels lists channels that exist locally.
func (s *Store) Channels() ([]string, error) {
	out, err := s.Git.Run("for-each-ref", "--format=%(refname)", s.ChannelsNamespace()+"/")
	if err != nil || out == "" {
		return nil, err
	}
	var names []string
	for _, ref := range strings.Split(out, "\n") {
		names = append(names, strings.TrimPrefix(ref, s.ChannelsNamespace()+"/"))
	}
	return names, nil
}

// renderMessage composes the stored commit message: body, blank line, trailers.
func renderMessage(body, by string, labels []string) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(body, "\n"))
	b.WriteString("\n\n")
	for _, l := range labels {
		fmt.Fprintf(&b, "Label: %s\n", l)
	}
	fmt.Fprintf(&b, "By: %s\n", by)
	return b.String()
}

// parseMessage splits a raw commit message back into body and trailers.
// Messages written by other tools may lack trailers; fallbackBy fills in.
func parseMessage(raw, fallbackBy string) (body, by string, labels []string) {
	raw = strings.TrimRight(raw, "\n")
	by = fallbackBy
	paras := strings.Split(raw, "\n\n")
	last := paras[len(paras)-1]
	trailerish := len(paras) > 1
	for _, line := range strings.Split(last, "\n") {
		if !strings.Contains(line, ": ") {
			trailerish = false
			break
		}
	}
	if !trailerish {
		return raw, by, nil
	}
	for _, line := range strings.Split(last, "\n") {
		switch {
		case strings.HasPrefix(line, "Label: "):
			labels = append(labels, strings.TrimPrefix(line, "Label: "))
		case strings.HasPrefix(line, "By: "):
			by = strings.TrimPrefix(line, "By: ")
		}
	}
	return strings.TrimRight(strings.Join(paras[:len(paras)-1], "\n\n"), "\n"), by, labels
}

// AppendMessage commits one message to a channel locally (compare-and-swap on
// the channel ref). It does not push; pair with SyncChannel.
func (s *Store) AppendMessage(channel, body, by string, labels []string) (string, error) {
	ref := s.ChannelRef(channel)
	emptyTree, err := s.Git.RunStdin("", "mktree")
	if err != nil {
		return "", err
	}
	parent, _ := s.Git.Run("rev-parse", "--verify", "--quiet", ref)
	args := []string{"commit-tree", emptyTree, "-m", renderMessage(body, by, labels)}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	sha, err := s.Git.Run(args...)
	if err != nil {
		return "", err
	}
	old := parent
	if old == "" {
		old = zeroSHA
	}
	if _, err := s.Git.Run("update-ref", ref, sha, old); err != nil {
		return "", &ConflictError{Ref: ref, Detail: "channel changed while posting"}
	}
	return sha, nil
}

// SyncChannel pushes a channel's local messages to the remote. Because
// messages are independent commits, local messages that lost a race are
// replayed onto the remote tip instead of conflicting — the channel converges.
func (s *Store) SyncChannel(remote, channel string) error {
	ref := s.ChannelRef(channel)
	for attempt := 0; attempt < 3; attempt++ {
		local, err := s.Git.Run("rev-parse", "--verify", "--quiet", ref)
		if err != nil || local == "" {
			return ErrNoMetadata
		}
		remoteSHA, err := s.RemoteSHA(remote, ref)
		if err != nil {
			return err
		}
		if remoteSHA == local {
			return nil
		}
		if remoteSHA != "" && !s.isAncestor(remoteSHA, local) {
			// Someone else posted concurrently: fetch their tip and replay our
			// local-only messages on top of it.
			if _, err := s.Git.Run("fetch", "--quiet", remote, ref); err != nil {
				return fmt.Errorf("fetching channel %s: %w", channel, err)
			}
			tip, err := s.Git.Run("rev-parse", "FETCH_HEAD^{commit}")
			if err != nil {
				return err
			}
			remoteSHA = tip
			if !s.isAncestor(tip, local) {
				ours, _ := s.Git.Run("rev-list", "--reverse", local, "^"+tip)
				for _, sha := range strings.Split(ours, "\n") {
					if sha == "" {
						continue
					}
					msg, err := s.Git.Run("log", "-1", "--format=%B", sha)
					if err != nil {
						return err
					}
					tree, err := s.Git.Run("rev-parse", sha+"^{tree}")
					if err != nil {
						return err
					}
					tip, err = s.Git.Run("commit-tree", tree, "-p", tip, "-m", msg)
					if err != nil {
						return err
					}
				}
				if _, err := s.Git.Run("update-ref", ref, tip, local); err != nil {
					return &ConflictError{Ref: ref, Detail: "channel changed while syncing"}
				}
				local = tip
			}
		}
		lease := ref + ":" + remoteSHA
		if remoteSHA == "" {
			lease = ref + ":"
		}
		if _, err := s.Git.Run("push", "--quiet", "--no-verify", "--force-with-lease="+lease, remote, local+":"+ref); err == nil {
			return nil
		}
		// Lost another race between ls-remote and push; loop and replay again.
	}
	return &ConflictError{Ref: ref, Detail: "could not sync channel after retries"}
}

// messageKey identifies a message by content, independent of its commit SHA
// (a replayed message gets a new SHA but the same key).
func messageKey(m Message) string {
	return m.By + "\x00" + m.Body + "\x00" + strings.Join(m.Labels, "\x00")
}

// PullChannel brings the local channel up to date with the remote tip
// remoteSHA (fast-forward, or replay-and-push when local holds unsent
// messages) and returns the messages new since `since` ("" = all), newest
// first, excluding replays of messages that were already local. It also
// returns the new local tip.
func (s *Store) PullChannel(remote, channel, remoteSHA, since string) ([]Message, string, error) {
	ref := s.ChannelRef(channel)
	local := s.ChannelTip(channel)
	var replayed map[string]bool
	switch {
	case remoteSHA == "" || remoteSHA == local:
		// nothing to pull
	case local == "" || s.isAncestor(local, remoteSHA):
		if _, err := s.Git.Run("fetch", "--quiet", remote, ref+":"+ref); err != nil {
			return nil, local, fmt.Errorf("fetching channel %s: %w", channel, err)
		}
	default:
		// Local has unsent messages. Fetch the remote tip into FETCH_HEAD
		// (leaves our ref alone), remember our local-only messages by
		// content, then let SyncChannel replay them onto the remote tip.
		if _, err := s.Git.Run("fetch", "--quiet", remote, ref); err != nil {
			return nil, local, fmt.Errorf("fetching channel %s: %w", channel, err)
		}
		fetched, err := s.Git.Run("rev-parse", "FETCH_HEAD^{commit}")
		if err != nil {
			return nil, local, err
		}
		ours, _ := s.logMessages(ref, fetched, 0)
		replayed = map[string]bool{}
		for _, m := range ours {
			replayed[messageKey(m)] = true
		}
		if err := s.SyncChannel(remote, channel); err != nil {
			return nil, local, err
		}
	}
	tip := s.ChannelTip(channel)
	if tip == since || tip == "" {
		return nil, tip, nil
	}
	msgs, err := s.MessagesSince(channel, since, 0)
	if err != nil {
		return nil, tip, err
	}
	if len(replayed) > 0 {
		kept := msgs[:0]
		for _, m := range msgs {
			if !replayed[messageKey(m)] {
				kept = append(kept, m)
			}
		}
		msgs = kept
	}
	return msgs, tip, nil
}

// DeleteChannel removes a channel locally and on the remote. Messages are
// gone from the remote; other clones keep their local copy until they delete
// it too.
func (s *Store) DeleteChannel(remote, channel string) error {
	ref := s.ChannelRef(channel)
	if s.ChannelTip(channel) != "" {
		if _, err := s.Git.Run("update-ref", "-d", ref); err != nil {
			return err
		}
	}
	remoteSHA, err := s.RemoteSHA(remote, ref)
	if err != nil {
		return err
	}
	if remoteSHA == "" {
		return nil
	}
	_, err = s.Git.Run("push", "--quiet", "--no-verify", remote, ":"+ref)
	return err
}

// FetchChannels fast-forwards local channel refs from the remote. Local-only
// messages make a ref non-fast-forwardable; those are reported, not clobbered
// (they merge on the next say/push). Returns the channels left behind.
func (s *Store) FetchChannels(remote string) ([]string, error) {
	ns := s.ChannelsNamespace()
	_, err := s.Git.Run("fetch", "--quiet", remote, fmt.Sprintf("%s/*:%s/*", ns, ns))
	if err == nil {
		return nil, nil
	}
	// Some refs were rejected (non-FF). Figure out which.
	out, lsErr := s.Git.Run("ls-remote", remote, ns+"/*")
	if lsErr != nil {
		return nil, fmt.Errorf("fetching channels: %w", err)
	}
	var behind []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], ns+"/")
		local, _ := s.Git.Run("rev-parse", "--verify", "--quiet", ns+"/"+name)
		if local != "" && local != fields[0] && !s.isAncestor(local, fields[0]) {
			behind = append(behind, name)
		}
	}
	return behind, nil
}

// ChannelTip returns the local tip of a channel, or "" when it has none.
func (s *Store) ChannelTip(channel string) string {
	sha, _ := s.Git.Run("rev-parse", "--verify", "--quiet", s.ChannelRef(channel))
	return sha
}

// RemoteChannels returns every channel on the remote with its tip SHA, in a
// single round trip and without transferring objects — cheap enough to poll.
func (s *Store) RemoteChannels(remote string) (map[string]string, error) {
	ns := s.ChannelsNamespace()
	out, err := s.Git.Run("ls-remote", remote, ns+"/*")
	if err != nil {
		return nil, fmt.Errorf("cannot reach remote %q: %w", remote, err)
	}
	tips := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.HasPrefix(fields[1], ns+"/") {
			tips[strings.TrimPrefix(fields[1], ns+"/")] = fields[0]
		}
	}
	return tips, nil
}

// Messages returns a channel's messages, newest first. limit <= 0 means all.
func (s *Store) Messages(channel string, limit int) ([]Message, error) {
	return s.MessagesSince(channel, "", limit)
}

// MessagesSince returns the messages reachable from the channel tip but not
// from since (a commit SHA; "" means all), newest first.
func (s *Store) MessagesSince(channel, since string, limit int) ([]Message, error) {
	ref := s.ChannelRef(channel)
	if _, err := s.Git.Run("rev-parse", "--verify", "--quiet", ref); err != nil {
		return nil, ErrNoMetadata
	}
	return s.logMessages(ref, since, limit)
}

// logMessages parses `git log ref ^since` into messages, newest first.
func (s *Store) logMessages(ref, since string, limit int) ([]Message, error) {
	args := []string{"log", "--format=%H%x00%aI%x00%an%x00%B%x1e"}
	if limit > 0 {
		args = append(args, fmt.Sprintf("-n%d", limit))
	}
	args = append(args, ref)
	if since != "" {
		args = append(args, "^"+since)
	}
	args = append(args, "--")
	out, err := s.Git.Run(args...)
	if err != nil {
		return nil, err
	}
	var msgs []Message
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		parts := strings.SplitN(rec, "\x00", 4)
		if len(parts) != 4 {
			continue
		}
		body, by, labels := parseMessage(parts[3], parts[2])
		msgs = append(msgs, Message{SHA: parts[0], At: parts[1], By: by, Labels: labels, Body: body})
	}
	return msgs, nil
}

// ReadDefs returns the shared label/channel definitions document.
func (s *Store) ReadDefs() (schema.Doc, string, error) {
	sha, err := s.Git.Run("rev-parse", "--verify", "--quiet", s.DefsRef()+"^{commit}")
	if err != nil || sha == "" {
		return nil, "", ErrNoMetadata
	}
	blob, err := s.Git.Run("cat-file", "blob", sha+":"+DefsFile)
	if err != nil {
		return nil, sha, err
	}
	doc, err := schema.Parse([]byte(blob))
	return doc, sha, err
}

// WriteDefs stores the definitions document (CAS against parent).
func (s *Store) WriteDefs(doc schema.Doc, parent, message string) (string, error) {
	data, err := doc.Marshal()
	if err != nil {
		return "", err
	}
	blobSHA, err := s.Git.RunStdin(string(data), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	treeSHA, err := s.Git.RunStdin(fmt.Sprintf("100644 blob %s\t%s\n", blobSHA, DefsFile), "mktree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", treeSHA, "-m", message}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	sha, err := s.Git.Run(args...)
	if err != nil {
		return "", err
	}
	old := parent
	if old == "" {
		old = zeroSHA
	}
	if _, err := s.Git.Run("update-ref", s.DefsRef(), sha, old); err != nil {
		return "", &ConflictError{Ref: s.DefsRef(), Detail: "definitions changed since they were read"}
	}
	return sha, nil
}
