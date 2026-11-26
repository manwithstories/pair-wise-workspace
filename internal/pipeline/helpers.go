package pipeline

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pairwise/relay/internal/cache"
	"github.com/pairwise/relay/internal/transport"
)

func itoa(v int) string { return strconv.Itoa(v) }

func fiveDigits(seg uint64) string { return fmt.Sprintf("%05d", seg) }

func transportSummary(s transport.Stats) string {
	return fmt.Sprintf("packets=%d duplicates=%d out_of_order=%d late=%d missing=%d recovered=%d gaps=%d",
		s.Packets, s.Duplicates, s.OutOfOrder, s.Late, s.Missing, s.Recovered, len(s.Gaps))
}

func cacheStatsSummary(s cache.Stats) string {
	return fmt.Sprintf("rehash=%d hits=%d misses=%d quarantined=%d", s.Rehashes, s.Hits, s.Misses, s.Quarantined)
}

func uniqueUint64(in []uint64) []uint64 {
	seen := make(map[uint64]bool, len(in))
	var out []uint64
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func joinStrings(parts []string) string { return strings.Join(parts, ",") }
