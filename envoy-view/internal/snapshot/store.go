// Package snapshot holds parsed config dumps server-side so the UI can browse
// one consistent view of a config.
//
// Keeping snapshots on the server rather than shipping the whole dump to the
// browser matters at real scale: a dump with tens of thousands of endpoints is
// far too large to send up front, and re-fetching from Envoy on every node
// click would show a moving target as the control plane pushes updates.
package snapshot

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/boweidu/envoy-view/internal/admin"
	"github.com/boweidu/envoy-view/internal/xds"
)

// Snapshot is one parsed config dump.
type Snapshot struct {
	ID        string
	Source    string
	CreatedAt time.Time
	SizeBytes int
	Index     *xds.Index

	// ServerInfo is Envoy's self-report, absent for uploaded dumps.
	ServerInfo *admin.ServerInfo
}

// Store is a bounded, expiring set of snapshots.
type Store struct {
	max int
	ttl time.Duration
	now func() time.Time

	mu    sync.Mutex
	byID  map[string]*Snapshot
	order []string // oldest first
}

// NewStore returns a store holding at most max snapshots, each expiring after
// ttl. A non-positive max or ttl disables that limit.
func NewStore(max int, ttl time.Duration) *Store {
	return &Store{
		max:  max,
		ttl:  ttl,
		now:  time.Now,
		byID: make(map[string]*Snapshot),
	}
}

// Put assigns an ID, stores the snapshot, and evicts anything over the limits.
func (s *Store) Put(snap *Snapshot) (*Snapshot, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	snap.ID = id
	snap.CreatedAt = s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.byID[id] = snap
	s.order = append(s.order, id)
	s.evictLocked()
	return snap, nil
}

// Get returns a snapshot, or false if it is unknown or expired.
func (s *Store) Get(id string) (*Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.evictLocked()
	snap, ok := s.byID[id]
	return snap, ok
}

// List returns the live snapshots, newest first.
func (s *Store) List() []*Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.evictLocked()
	out := make([]*Snapshot, 0, len(s.order))
	for _, id := range slices.Backward(s.order) {
		out = append(out, s.byID[id])
	}
	return out
}

// Delete drops a snapshot. Missing IDs are not an error.
func (s *Store) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeLocked(id)
}

// Len returns the number of live snapshots.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	return len(s.byID)
}

// evictLocked drops expired snapshots, then the oldest until the count fits.
func (s *Store) evictLocked() {
	if s.ttl > 0 {
		cutoff := s.now().Add(-s.ttl)
		for _, id := range append([]string(nil), s.order...) {
			if snap, ok := s.byID[id]; ok && snap.CreatedAt.Before(cutoff) {
				s.removeLocked(id)
			}
		}
	}
	if s.max > 0 {
		for len(s.order) > s.max {
			s.removeLocked(s.order[0])
		}
	}
}

func (s *Store) removeLocked(id string) {
	if _, ok := s.byID[id]; !ok {
		return
	}
	delete(s.byID, id)
	for i, v := range s.order {
		if v == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// newID returns an unguessable snapshot ID. Snapshots contain the full proxy
// configuration, so IDs should not be enumerable even though the server is
// expected to be bound to localhost.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate snapshot id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
