// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

// Package session tracks the proxy's client-facing MCP sessions and their
// lazily-opened backend sessions.
//
// The proxy owns the client session: it mints a proxy session id (proxySID) at
// initialize and maps it to one backend session id per backend the caller
// touches (proxySID -> {backend name -> backend Mcp-Session-Id}). State is
// in-memory, so multi-replica deployments require sticky routing on the
// proxySID and a restart drops sessions (clients re-initialize).
package session

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// Session is a client-facing MCP session and its per-backend backend sessions.
type Session struct {
	ID       string
	created  time.Time
	lastSeen time.Time
	// backendSID maps a backend name to the Mcp-Session-Id issued by that
	// backend. Empty until the caller's first tools/call to that backend.
	backendSID map[string]string
}

// BackendSID returns the backend session id for a backend name, if known.
func (s *Session) BackendSID(backend string) (string, bool) {
	v, ok := s.backendSID[backend]
	return v, ok
}

// Store is a thread-safe set of sessions with idle TTL reaping.
type Store struct {
	mu  sync.Mutex
	m   map[string]*Session
	ttl time.Duration
	now func() time.Time // injectable clock for tests
}

// NewStore creates a session store. idleTTL <= 0 disables reaping.
func NewStore(idleTTL time.Duration) *Store {
	return &Store{m: make(map[string]*Session), ttl: idleTTL, now: time.Now}
}

// Mint creates a new session with a fresh random proxySID.
func (s *Store) Mint() *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked()
	now := s.now()
	sess := &Session{
		ID:         newID(),
		created:    now,
		lastSeen:   now,
		backendSID: make(map[string]string),
	}
	s.m[sess.ID] = sess
	return sess
}

// Get returns the session for a proxySID and refreshes its last-seen time.
// The second return is false for unknown/expired ids.
func (s *Store) Get(id string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked()
	sess, ok := s.m[id]
	if !ok {
		return nil, false
	}
	sess.lastSeen = s.now()
	return sess, true
}

// SetBackendSID records the backend session id for (proxySID, backend). It is a
// no-op for an unknown proxySID.
func (s *Store) SetBackendSID(proxySID, backend, backendSID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.m[proxySID]; ok {
		sess.backendSID[backend] = backendSID
		sess.lastSeen = s.now()
	}
}

// Delete removes a session and returns it (so the caller can tear down the
// mapped backend sessions). The second return is false if it was unknown.
func (s *Store) Delete(id string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok {
		return nil, false
	}
	delete(s.m, id)
	return sess, true
}

// Len returns the number of live sessions (after reaping). Intended for tests.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked()
	return len(s.m)
}

// reapLocked drops sessions idle longer than ttl. Caller holds s.mu.
func (s *Store) reapLocked() {
	if s.ttl <= 0 {
		return
	}
	cutoff := s.now().Add(-s.ttl)
	for id, sess := range s.m {
		if sess.lastSeen.Before(cutoff) {
			delete(s.m, id)
		}
	}
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
