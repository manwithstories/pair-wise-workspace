package peer

import (
	"errors"
	"testing"

	"relayctl/internal/signal"
)

const loopbackCandidate = "candidate:1 1 UDP 2122260223 127.0.0.1 5000 typ host"
const loopbackCandidateTwo = "candidate:2 1 UDP 2122260222 127.0.0.1 5001 typ host"
const bannedCandidate = "candidate:3 1 UDP 2122260221 192.0.2.10 5002 typ host"

func TestParseCandidateLoopbackPolicy(t *testing.T) {
	candidate, err := ParseCandidate(loopbackCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Host != "127.0.0.1" || candidate.Port != 5000 {
		t.Fatalf("candidate = %+v", candidate)
	}
	if _, err := ParseCandidate(bannedCandidate); !errors.Is(err, ErrNonLoopbackCandidate) {
		t.Fatalf("error = %v", err)
	}
	if _, err := ParseCandidate("not-a-candidate"); !errors.Is(err, signal.ErrInvalidMessage) {
		t.Fatalf("malformed error = %v", err)
	}
}

func TestCandidateSetCountsRejectedCandidates(t *testing.T) {
	set := NewCandidateSet()
	if _, err := set.AddLocal(loopbackCandidate); err != nil {
		t.Fatal(err)
	}
	if _, err := set.AddRemote(loopbackCandidateTwo); err != nil {
		t.Fatal(err)
	}
	if _, err := set.AddRemote(bannedCandidate); !errors.Is(err, ErrNonLoopbackCandidate) {
		t.Fatalf("error = %v", err)
	}
	local, remote, banned := set.Snapshot()
	if len(local) != 1 || len(remote) != 1 || banned != 1 {
		t.Fatalf("local=%d remote=%d banned=%d", len(local), len(remote), banned)
	}
}

func TestEnumerateRawPairs(t *testing.T) {
	pairs, err := EnumerateRawPairs(
		[]string{loopbackCandidate, loopbackCandidateTwo},
		[]string{loopbackCandidateTwo, loopbackCandidate},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 4 {
		t.Fatalf("pairs = %d, want 4", len(pairs))
	}
	if _, err := EnumerateRawPairs([]string{loopbackCandidate}, []string{bannedCandidate}); !errors.Is(err, ErrNonLoopbackCandidate) {
		t.Fatalf("error = %v", err)
	}
}
