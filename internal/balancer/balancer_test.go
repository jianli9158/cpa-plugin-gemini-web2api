package balancer

import (
	"testing"
	"time"

	"github.com/Mag1cFall/Gemini-Web2API/internal/gemini"
)

func TestNextForPinnedPreservesSessionBinding(t *testing.T) {
	pool := NewAccountPool(time.Minute, 10*time.Minute)
	clientA := &gemini.Client{}
	clientB := &gemini.Client{}
	pool.entries = []*accountEntry{
		{AccountID: "accA", Client: clientA, State: StateReady},
		{AccountID: "accB", Client: clientB, State: StateReady},
	}

	gotClient, gotID := pool.nextForPinned("accA", "s1", "", nil)
	if gotClient != clientA || gotID != "accA" {
		t.Fatalf("first turn: got (%p, %q), want (%p, accA)", gotClient, gotID, clientA)
	}

	// CPA core round-robins to accB on the next turn; existing session s1 must stay on accA.
	gotClient, gotID = pool.nextForPinned("accB", "s1", "", nil)
	if gotClient != clientA || gotID != "accA" {
		t.Fatalf("second turn with different pinnedID: got (%p, %q), want (%p, accA)", gotClient, gotID, clientA)
	}

	// If accA is excluded, nextForPinned returns (nil, "accA") so the caller can rebuild on another account or report 503.
	gotClient, gotID = pool.nextForPinned("accB", "s1", "", map[string]struct{}{"accA": {}})
	if gotClient != nil || gotID != "accA" {
		t.Fatalf("excluded bound account: got (%p, %q), want (nil, accA)", gotClient, gotID)
	}

	// Rebuilt / new session (empty sessionKey) uses the new pinnedID accB.
	gotClient, gotID = pool.nextForPinned("accB", "", "", map[string]struct{}{"accA": {}})
	if gotClient != clientB || gotID != "accB" {
		t.Fatalf("rebuilt session: got (%p, %q), want (%p, accB)", gotClient, gotID, clientB)
	}
}
