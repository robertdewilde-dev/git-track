package store

import (
	"fmt"
	"strings"
)

// PushResult describes the outcome for one ref.
type PushResult struct {
	Branch string `json:"branch"`
	Ref    string `json:"ref"`
	Status string `json:"status"` // "pushed", "up-to-date", "created"
}

// RemoteSHA returns the SHA a ref points to on the remote, or "" if absent.
func (s *Store) RemoteSHA(remote, ref string) (string, error) {
	out, err := s.Git.Run("ls-remote", remote, ref)
	if err != nil {
		return "", fmt.Errorf("cannot reach remote %q: %w", remote, err)
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == ref {
			return fields[0], nil
		}
	}
	return "", nil
}

// Push pushes one branch's metadata ref to the remote with force-with-lease
// semantics. The lease is the remote SHA observed via ls-remote; a push is
// only attempted when that SHA is already an ancestor of the local state, so
// remote history is never discarded. A non-fast-forward situation returns
// *ConflictError.
func (s *Store) Push(remote, branch string) (PushResult, error) {
	ref := s.Ref(branch)
	res := PushResult{Branch: branch, Ref: ref}
	local, err := s.Git.Run("rev-parse", "--verify", "--quiet", ref)
	if err != nil || local == "" {
		return res, ErrNoMetadata
	}
	remoteSHA, err := s.RemoteSHA(remote, ref)
	if err != nil {
		return res, err
	}
	lease := ref + ":"
	switch {
	case remoteSHA == local:
		res.Status = "up-to-date"
		return res, nil
	case remoteSHA == "":
		res.Status = "created"
		// empty expect: the ref must not exist on the remote yet
	default:
		if !s.isAncestor(remoteSHA, local) {
			return res, &ConflictError{Ref: ref, Detail: "remote metadata has changes not present locally"}
		}
		res.Status = "pushed"
		lease = ref + ":" + remoteSHA
	}
	_, err = s.Git.Run("push", "--quiet", "--no-verify", "--force-with-lease="+lease, remote, local+":"+ref)
	if err != nil {
		// Lost a race between ls-remote and push.
		return res, &ConflictError{Ref: ref, Detail: "push rejected (stale info)"}
	}
	return res, nil
}

// isAncestor reports whether ancestor is reachable from descendant. Unknown
// objects (e.g. a remote SHA we never fetched) count as not an ancestor.
func (s *Store) isAncestor(ancestor, descendant string) bool {
	_, err := s.Git.Run("merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// Fetch fetches all metadata refs from the remote. The forced refspec mirrors
// the remote namespace state into the local refs.
func (s *Store) Fetch(remote string) error {
	spec := fmt.Sprintf("+%s/*:%s/*", s.Namespace, s.Namespace)
	_, err := s.Git.Run("fetch", "--quiet", remote, spec)
	if err != nil {
		return fmt.Errorf("fetching metadata refs: %w", err)
	}
	return nil
}

// DeleteRemote deletes a branch's metadata ref on the remote.
func (s *Store) DeleteRemote(remote, branch string) error {
	_, err := s.Git.Run("push", "--quiet", "--no-verify", remote, ":"+s.Ref(branch))
	return err
}
