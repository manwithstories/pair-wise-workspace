package peer

import (
	"net"
	"strings"
)

// candidateFields splits an ICE candidate string into its fields.
func candidateFields(cand string) []string {
	s := strings.TrimPrefix(strings.TrimSpace(cand), "a=")
	s = strings.TrimPrefix(s, "candidate:")
	return strings.Fields(s)
}

// CandidateIP extracts the connection address from an ICE candidate string
// ("candidate:... ip port typ ..."), tolerating an "a=" prefix.
func CandidateIP(cand string) net.IP {
	f := candidateFields(cand)
	if len(f) < 6 {
		return nil
	}
	return net.ParseIP(f[4])
}

// CandidateFilter accepts unique loopback candidates and counts everything
// else as filtered. It is used both for trickled candidates and for
// statistics.
type CandidateFilter struct {
	seen     map[string]struct{}
	accepted int
	filtered int
}

// NewCandidateFilter returns an empty filter.
func NewCandidateFilter() *CandidateFilter {
	return &CandidateFilter{seen: map[string]struct{}{}}
}

// Add reports whether cand is a new loopback RTP-component candidate.
// Duplicates and non-loopback or unparseable candidates are rejected and
// counted; non-RTP components (rtcp-mux is in use) are ignored entirely.
func (f *CandidateFilter) Add(cand string) bool {
	if fields := candidateFields(cand); len(fields) >= 2 && fields[1] != "1" {
		return false
	}
	key := strings.TrimSpace(cand)
	if _, dup := f.seen[key]; dup {
		f.filtered++
		return false
	}
	f.seen[key] = struct{}{}
	ip := CandidateIP(cand)
	if ip == nil || !ip.IsLoopback() {
		f.filtered++
		return false
	}
	f.accepted++
	return true
}

// Accepted returns the number of accepted loopback candidates.
func (f *CandidateFilter) Accepted() int { return f.accepted }

// Filtered returns the number of rejected candidates.
func (f *CandidateFilter) Filtered() int { return f.filtered }

// Pairs computes the number of candidate pairs for one leg from the
// accepted candidate counts of both ends, counting both check directions.
func Pairs(local, remote int) int { return 2 * local * remote }
