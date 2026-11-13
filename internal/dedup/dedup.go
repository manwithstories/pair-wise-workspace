package dedup

type Kind string

const (
	KindNew       Kind = "new"
	KindRevision  Kind = "revision"
	KindDuplicate Kind = "duplicate"
)

type Existing struct {
	AnnouncementID int64
	BusinessKey    string
	ContentHash    string
	CanonicalURL   string
	KnownHashes    map[string]bool
}

type Candidate struct {
	BusinessKey  string
	ContentHash  string
	CanonicalURL string
}

type Decision struct {
	Kind   Kind
	Reason string
}

// Decide implements the two-level identity rule. A content change under the
// same business key wins over URL duplication so same-URL revisions are
// versioned instead of being silently discarded.
func Decide(candidate Candidate, byBusinessKey, byURL *Existing) Decision {
	if byBusinessKey != nil {
		if candidate.ContentHash != "" && byBusinessKey.ContentHash != candidate.ContentHash {
			if byBusinessKey.KnownHashes[candidate.ContentHash] {
				return Decision{Kind: KindDuplicate, Reason: "historical version already stored"}
			}
			return Decision{Kind: KindRevision, Reason: "business key content changed"}
		}
		if candidate.CanonicalURL != "" && byBusinessKey.CanonicalURL == candidate.CanonicalURL {
			return Decision{Kind: KindDuplicate, Reason: "canonical URL already stored"}
		}
		return Decision{Kind: KindDuplicate, Reason: "business key forwarded by another site"}
	}
	if byURL != nil {
		return Decision{Kind: KindDuplicate, Reason: "canonical URL already stored"}
	}
	return Decision{Kind: KindNew, Reason: "first observation"}
}
