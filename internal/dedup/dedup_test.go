package dedup

import "testing"

func TestDecide(t *testing.T) {
	candidate := Candidate{BusinessKey: "P-1", ContentHash: "h1", CanonicalURL: "u1"}
	if got := Decide(candidate, nil, nil); got.Kind != KindNew {
		t.Fatalf("new: %+v", got)
	}
	if got := Decide(candidate, nil, &Existing{AnnouncementID: 1}); got.Kind != KindDuplicate {
		t.Fatalf("url duplicate: %+v", got)
	}
	existing := &Existing{AnnouncementID: 1, ContentHash: "h1", KnownHashes: map[string]bool{"h1": true}}
	if got := Decide(candidate, existing, nil); got.Kind != KindDuplicate {
		t.Fatalf("same content: %+v", got)
	}
	candidate.ContentHash = "h2"
	existing.ContentHash = "h1"
	if got := Decide(candidate, existing, nil); got.Kind != KindRevision {
		t.Fatalf("revision: %+v", got)
	}
	existing.KnownHashes["h2"] = true
	if got := Decide(candidate, existing, nil); got.Kind != KindDuplicate {
		t.Fatalf("historical replay: %+v", got)
	}
}
