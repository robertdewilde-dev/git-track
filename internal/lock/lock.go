// Package lock implements the agent.lockedBy distributed mutex: lock identity,
// TTL expiry, and actor comparison. Acquisition/release ordering lives in the
// CLI layer; the actual mutual exclusion comes from force-with-lease pushes.
package lock

import (
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/robertdewilde-dev/git-track/internal/schema"
)

// Info describes a held lock.
type Info struct {
	Owner string        // "<user>@<machine>:<pid>"
	At    time.Time     // when acquired (zero if unknown)
	TTL   time.Duration // 0 means no expiry
}

// Expired reports whether the lock's TTL has elapsed.
func (i *Info) Expired(now time.Time) bool {
	return i.TTL > 0 && !i.At.IsZero() && now.After(i.At.Add(i.TTL))
}

// FromDoc extracts lock info from a document, or nil if unlocked.
func FromDoc(d schema.Doc) *Info {
	v, ok := d.Get("agent.lockedBy")
	if !ok || v == nil {
		return nil
	}
	owner, ok := v.(string)
	if !ok || owner == "" {
		return nil
	}
	info := &Info{Owner: owner}
	if at, ok := d.Get("agent.lockedAt"); ok {
		if s, ok := at.(string); ok {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				info.At = t
			}
		}
	}
	if ttl, ok := d.Get("agent.lockTtl"); ok {
		if s, ok := ttl.(string); ok {
			if dur, err := time.ParseDuration(s); err == nil {
				info.TTL = dur
			}
		}
	}
	return info
}

// Active reports whether the document holds a live (non-expired) lock.
func Active(d schema.Doc, now time.Time) *Info {
	info := FromDoc(d)
	if info == nil || info.Expired(now) {
		return nil
	}
	return info
}

// Username returns the OS user name.
func Username() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		name := u.Username
		// Windows reports DOMAIN\user.
		if i := strings.LastIndexByte(name, '\\'); i >= 0 {
			name = name[i+1:]
		}
		return name
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return "unknown"
}

// Machine returns the short hostname.
func Machine() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}

// Actor identifies the current user+machine ("robert@robert-desktop").
func Actor() string {
	return Username() + "@" + Machine()
}

// Value is the full lock value for this process ("user@machine:pid").
func Value() string {
	return fmt.Sprintf("%s:%d", Actor(), os.Getpid())
}

// SameActor reports whether owner belongs to the current user+machine
// (the pid suffix is ignored: any process of the same actor may pass).
func SameActor(owner string) bool {
	base := owner
	if i := strings.LastIndexByte(owner, ':'); i >= 0 {
		base = owner[:i]
	}
	return base == Actor()
}
