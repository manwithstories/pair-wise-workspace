package testkit

import (
	"encoding/json"
	"os"

	"archivearchivist/internal/model"
)

// rand is a small xorshift generator so fixtures do not depend on math/rand
// implementation details.
type rand struct{ s uint64 }

func newRand(seed int64) *rand {
	s := uint64(seed) ^ 0x9e3779b97f4a7c15
	if s == 0 {
		s = 1
	}
	return &rand{s: s}
}

func (r *rand) next() uint64 {
	r.s ^= r.s << 13
	r.s ^= r.s >> 7
	r.s ^= r.s << 17
	return r.s
}

func (r *rand) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

func writeTimeline(path string, tl *model.Timeline) error {
	return writeJSON(path, tl)
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// writeJSON writes indented JSON with stable field order.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}
