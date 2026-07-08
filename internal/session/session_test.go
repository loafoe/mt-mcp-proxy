// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"testing"
	"time"
)

func TestMintAndGet(t *testing.T) {
	s := NewStore(0)
	sess := s.Mint()
	if sess.ID == "" {
		t.Fatal("expected a session id")
	}
	got, ok := s.Get(sess.ID)
	if !ok || got.ID != sess.ID {
		t.Fatal("session not retrievable")
	}
	if _, ok := s.Get("nope"); ok {
		t.Error("unknown id must not resolve")
	}
}

func TestBackendSIDMapping(t *testing.T) {
	s := NewStore(0)
	sess := s.Mint()
	if _, ok := sess.BackendSID("b1"); ok {
		t.Error("no backend sid expected yet")
	}
	s.SetBackendSID(sess.ID, "b1", "back-1")
	got, _ := s.Get(sess.ID)
	if sid, ok := got.BackendSID("b1"); !ok || sid != "back-1" {
		t.Errorf("backend sid not stored, got %q ok=%v", sid, ok)
	}
}

func TestDelete(t *testing.T) {
	s := NewStore(0)
	sess := s.Mint()
	s.SetBackendSID(sess.ID, "b1", "back-1")
	deleted, ok := s.Delete(sess.ID)
	if !ok {
		t.Fatal("delete should report the session existed")
	}
	if sid, _ := deleted.BackendSID("b1"); sid != "back-1" {
		t.Error("deleted session should still expose its backend sids for teardown")
	}
	if _, ok := s.Get(sess.ID); ok {
		t.Error("session should be gone")
	}
}

func TestIdleReaping(t *testing.T) {
	s := NewStore(time.Minute)
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	sess := s.Mint()

	now = now.Add(2 * time.Minute) // idle past ttl
	if _, ok := s.Get(sess.ID); ok {
		t.Error("idle session should have been reaped")
	}
	if s.Len() != 0 {
		t.Errorf("store should be empty, got %d", s.Len())
	}
}
