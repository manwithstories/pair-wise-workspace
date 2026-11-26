package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"sync"
)

// Record is one idempotent node result keyed by stable input hash.
type Record struct {
	NodeID     string `json:"node_id"`
	InputHash  string `json:"input_hash"`
	OutputHash string `json:"output_hash"`
	Output     []byte `json:"output,omitempty"`
	Status     string `json:"status"`
	Attempts   int    `json:"attempts"`
}

// Journal is a deterministic, optionally persistent result ledger.
type Journal struct {
	mu      sync.Mutex
	Records map[string]Record `json:"records"`
}

// NewJournal creates an empty in-memory journal.
func NewJournal() *Journal {
	return &Journal{Records: make(map[string]Record)}
}

// LoadJournal reads a JSON journal; a missing file yields an empty journal.
func LoadJournal(path string) (*Journal, error) {
	j := NewJournal()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return j, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return j, nil
	}
	var doc struct {
		Records []Record `json:"records"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	j.Records = make(map[string]Record, len(doc.Records))
	for _, rec := range doc.Records {
		j.Records[rec.NodeID] = rec
	}
	return j, nil
}

// Get returns a copy of a node record.
func (j *Journal) Get(nodeID string) (Record, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.Records[nodeID]
	return r, ok
}

// RecordSuccess stores a successful result only after its output hash is known.
func (j *Journal) RecordSuccess(nodeID, inputHash string, output []byte, attempts int) Record {
	outHash := HashBytes(output)
	r := Record{
		NodeID: nodeID, InputHash: inputHash, OutputHash: outHash,
		Output: append([]byte(nil), output...), Status: "success", Attempts: attempts,
	}
	j.mu.Lock()
	j.Records[nodeID] = r
	j.mu.Unlock()
	return r
}

// Save writes canonical, stable JSON for recovery across process restarts.
func (j *Journal) Save(path string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	ids := make([]string, 0, len(j.Records))
	for id := range j.Records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ordered := struct {
		Records []Record `json:"records"`
	}{Records: make([]Record, 0, len(ids))}
	for _, id := range ids {
		ordered.Records = append(ordered.Records, j.Records[id])
	}
	raw, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

// HashBytes returns the lowercase SHA-256 hex digest of b.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// InputHash builds a stable hash from ordered input fragments. Callers should
// pass dependency hashes in sorted or declared-stable order.
func InputHash(nodeID string, ownInput []byte, depHashes map[string]string) string {
	h := sha256.New()
	writeField := func(label string, b []byte) {
		_, _ = h.Write([]byte(label))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(itob(len(b)))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(b)
		_, _ = h.Write([]byte{0})
	}
	writeField("node", []byte(nodeID))
	writeField("input", ownInput)
	ids := make([]string, 0, len(depHashes))
	for id := range depHashes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		writeField("dep:"+id, []byte(depHashes[id]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func itob(n int) []byte {
	var buf [8]byte
	v := uint64(n)
	for i := 7; i >= 0; i-- {
		buf[i] = byte(v)
		v >>= 8
	}
	return buf[:]
}
