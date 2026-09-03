// Package store reads and writes metadata documents as commit objects under a
// ref namespace (default refs/meta/branches/<branch>). Each write creates a
// commit whose tree holds a single meta.json blob, parented on the previous
// state, and updates the ref with compare-and-swap semantics.
package store

import (
	"errors"
	"fmt"
	"strings"

	"github.com/robertdewilde-dev/git-track/internal/gitcmd"
	"github.com/robertdewilde-dev/git-track/internal/schema"
)

// DefaultNamespace is the default ref prefix, overridable via
// `git config track.namespace`.
const DefaultNamespace = "refs/meta/branches"

// MetaFile is the single file each metadata commit's tree contains.
const MetaFile = "meta.json"

const zeroSHA = "0000000000000000000000000000000000000000"

// ErrNoMetadata means the branch has no metadata ref.
var ErrNoMetadata = errors.New("no metadata for this branch")

// ConflictError means a ref update lost a race (local CAS failure or a
// non-fast-forward push rejection).
type ConflictError struct {
	Ref    string
	Detail string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("ref conflict on %s: %s (run `git track fetch` and retry)", e.Ref, e.Detail)
}

// Store accesses metadata refs in one repository.
type Store struct {
	Git       *gitcmd.Runner
	Namespace string // e.g. "refs/meta/branches", no trailing slash
}

// New builds a Store, resolving the namespace from git config.
func New(g *gitcmd.Runner) *Store {
	ns := g.Config("track.namespace")
	if ns == "" {
		ns = DefaultNamespace
	}
	return &Store{Git: g, Namespace: strings.TrimSuffix(ns, "/")}
}

// Ref returns the metadata ref for a branch.
func (s *Store) Ref(branch string) string {
	return s.Namespace + "/" + branch
}

// Read returns the document and the metadata commit SHA for a branch.
// Returns ErrNoMetadata if the ref does not exist.
func (s *Store) Read(branch string) (schema.Doc, string, error) {
	sha, err := s.Git.Run("rev-parse", "--verify", "--quiet", s.Ref(branch)+"^{commit}")
	if err != nil || sha == "" {
		return nil, "", ErrNoMetadata
	}
	return s.readAt(sha)
}

func (s *Store) readAt(commit string) (schema.Doc, string, error) {
	blob, err := s.Git.Run("cat-file", "blob", commit+":"+MetaFile)
	if err != nil {
		return nil, commit, fmt.Errorf("metadata commit %s has no %s: %w", commit, MetaFile, err)
	}
	doc, err := schema.Parse([]byte(blob))
	if err != nil {
		return nil, commit, err
	}
	return doc, commit, nil
}

// Write stores doc as a new metadata commit for branch. parent is the commit
// SHA the caller read (empty for a first write); the ref update is
// compare-and-swapped against it, returning *ConflictError if the ref moved.
func (s *Store) Write(branch string, doc schema.Doc, parent, message string) (string, error) {
	data, err := doc.Marshal()
	if err != nil {
		return "", err
	}
	blobSHA, err := s.Git.RunStdin(string(data), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", fmt.Errorf("writing metadata blob: %w", err)
	}
	treeSHA, err := s.Git.RunStdin(fmt.Sprintf("100644 blob %s\t%s\n", blobSHA, MetaFile), "mktree")
	if err != nil {
		return "", fmt.Errorf("writing metadata tree: %w", err)
	}
	args := []string{"commit-tree", treeSHA, "-m", message}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	commitSHA, err := s.Git.Run(args...)
	if err != nil {
		return "", fmt.Errorf("writing metadata commit: %w", err)
	}
	old := parent
	if old == "" {
		old = zeroSHA // ref must not exist yet
	}
	if _, err := s.Git.Run("update-ref", s.Ref(branch), commitSHA, old); err != nil {
		return "", &ConflictError{Ref: s.Ref(branch), Detail: "metadata changed since it was read"}
	}
	return commitSHA, nil
}

// Squash replaces the metadata history with a single parentless commit
// holding doc, compare-and-swapped against expectedOld (the current commit).
func (s *Store) Squash(branch string, doc schema.Doc, expectedOld, message string) (string, error) {
	data, err := doc.Marshal()
	if err != nil {
		return "", err
	}
	blobSHA, err := s.Git.RunStdin(string(data), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	treeSHA, err := s.Git.RunStdin(fmt.Sprintf("100644 blob %s\t%s\n", blobSHA, MetaFile), "mktree")
	if err != nil {
		return "", err
	}
	commitSHA, err := s.Git.Run("commit-tree", treeSHA, "-m", message)
	if err != nil {
		return "", err
	}
	if _, err := s.Git.Run("update-ref", s.Ref(branch), commitSHA, expectedOld); err != nil {
		return "", &ConflictError{Ref: s.Ref(branch), Detail: "metadata changed since it was read"}
	}
	return commitSHA, nil
}

// Delete removes the metadata ref for a branch.
func (s *Store) Delete(branch string) error {
	_, err := s.Git.Run("update-ref", "-d", s.Ref(branch))
	return err
}

// Branches lists all branches that have local metadata refs.
func (s *Store) Branches() ([]string, error) {
	out, err := s.Git.Run("for-each-ref", "--format=%(refname)", s.Namespace+"/")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	var branches []string
	for _, ref := range strings.Split(out, "\n") {
		branches = append(branches, strings.TrimPrefix(ref, s.Namespace+"/"))
	}
	return branches, nil
}

// LogEntry is one metadata change.
type LogEntry struct {
	SHA     string `json:"sha"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

// Log returns the metadata history for a branch, newest first.
func (s *Store) Log(branch string) ([]LogEntry, error) {
	if _, err := s.Git.Run("rev-parse", "--verify", "--quiet", s.Ref(branch)); err != nil {
		return nil, ErrNoMetadata
	}
	out, err := s.Git.Run("log", "--format=%H%x00%aI%x00%s", s.Ref(branch), "--")
	if err != nil {
		return nil, err
	}
	var entries []LogEntry
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\x00", 3)
		if len(parts) == 3 {
			entries = append(entries, LogEntry{SHA: parts[0], Date: parts[1], Subject: parts[2]})
		}
	}
	return entries, nil
}

// ReadAtCommit returns the document stored in a specific metadata commit.
func (s *Store) ReadAtCommit(commit string) (schema.Doc, error) {
	doc, _, err := s.readAt(commit)
	return doc, err
}
