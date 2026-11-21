package index

import (
	"os"
	"testing"
)

func readDirNames(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names, nil
}

// tamperStoreFile flips one byte inside a persisted index to prove the
// checksum guard rejects partial/corrupted state on restart.
func tamperStoreFile(t *testing.T, s *Store, name string) {
	t.Helper()
	path := s.Path(name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte inside the committed_samples region (after the header).
	idx := len(data) / 2
	data[idx] ^= 0xFF
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
