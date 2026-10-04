package desktoprelay

import (
	"fmt"
	"testing"
	"time"
)

func TestPruneSessionsForgetsIdleAndCapsTheRest(t *testing.T) {
	now := time.Now()
	s := diskState{Sessions: map[string]Session{}}
	add := func(id string, age time.Duration, inFlight uint64) {
		s.Sessions[id] = Session{SessionID: id, LastSeen: now.Add(-age), InFlight: inFlight}
	}
	add("old-idle", 15*24*time.Hour, 0)
	add("old-running", 15*24*time.Hour, 1)
	for i := 0; i < maxSessions+10; i++ {
		add(fmt.Sprint("recent-", i), time.Duration(i)*time.Second, 0)
	}
	pruneSessions(&s, now)
	if _, ok := s.Sessions["old-idle"]; ok {
		t.Fatal("idle session older than the TTL was kept")
	}
	if _, ok := s.Sessions["old-running"]; !ok {
		t.Fatal("running session was pruned")
	}
	if _, ok := s.Sessions["recent-0"]; !ok || len(s.Sessions) != maxSessions+1 {
		t.Fatalf("kept %d sessions", len(s.Sessions))
	}
}
