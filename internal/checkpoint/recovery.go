package checkpoint

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"strconv"

	"avpipe/internal/media"
)

// ReadJournalSeqs returns complete chunk sequence numbers in the append-only
// journal. A torn final line from a hard stop is skipped because the atomic
// chunk file and index are the commit source.
func ReadJournalSeqs(path string) ([]int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var seqs []int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		var rec ChunkRecord
		if json.Unmarshal(sc.Bytes(), &rec) == nil {
			seqs = append(seqs, rec.Seq)
		}
	}
	return seqs, sc.Err()
}

// CheckJournal ensures every committed index chunk has a durable journal row
// and that every complete journal row is represented in the recovered index.
func (s *Store) CheckJournal(idx *Index) error {
	seqs, err := ReadJournalSeqs(s.Journal)
	if err != nil {
		return err
	}
	journalSet := map[int]bool{}
	for _, seq := range seqs {
		journalSet[seq] = true
		if idx.Chunks[seq] == nil {
			return &media.Error{Code: media.CodeCheckpoint, Message: "journal chunk missing from index: " + strconv.Itoa(seq)}
		}
	}
	for seq := range idx.Chunks {
		if !journalSet[seq] {
			return &media.Error{Code: media.CodeCheckpoint, Message: "committed chunk missing journal row: " + strconv.Itoa(seq)}
		}
	}
	return nil
}

// Reconcile loads the index, verifies segment hashes and removes orphans.
func (s *Store) Reconcile() (*Index, int, []string, []string, error) {
	idx, err := s.LoadIndex()
	if err != nil {
		return nil, 0, nil, nil, err
	}
	if err := s.CheckJournal(idx); err != nil {
		return idx, 0, nil, nil, err
	}
	verified, bad, err := s.Verify(idx)
	if err != nil {
		return idx, verified, bad, nil, err
	}
	if len(bad) > 0 {
		return idx, verified, bad, nil, errors.New("committed checkpoint verification failed")
	}
	removed, err := s.CleanOrphans(idx)
	return idx, verified, bad, removed, err
}
