package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// SourceID builds the stable cache identity for a seeded fixture.
func SourceID(seed int64, faults []string) string {
	names := append([]string(nil), faults...)
	sort.Strings(names)
	return fmt.Sprintf("fixture/v1/seed=%d/faults=%s", seed, join(names, ","))
}

func join(items []string, sep string) string { return strings.Join(items, sep) }

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
