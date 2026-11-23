// Package provenance records where every output came from: the input hash,
// the repairs applied to it, and the manifest entry that produced it. A
// complete chain lets an operator walk backwards from an output segment to
// the exact playlist entry and the bytes that fed it.
package provenance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"mediarun/internal/clock"
)

// Decision is one repair or transformation applied to an input.
type Decision struct {
	Stage  string `json:"stage"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
	// Count is how many observations the decision affected.
	Count int `json:"count,omitempty"`
}

// Entry links one output artefact to its inputs.
type Entry struct {
	Output      string     `json:"output"`
	OutputHash  string     `json:"output_hash"`
	ManifestURI string     `json:"manifest_uri"`
	InputHashes []string   `json:"input_hashes"`
	Decisions   []Decision `json:"decisions,omitempty"`
	// SegmentIDs lists the manifest segments included in the output.
	SegmentIDs []string `json:"segment_ids,omitempty"`
	Stage      string   `json:"stage"`
}

// Trace is an append-only provenance ledger.
type Trace struct {
	entries []Entry
	seen    map[string]struct{}
}

// NewTrace creates an empty ledger.
func NewTrace() *Trace {
	return &Trace{seen: map[string]struct{}{}}
}

// Record appends one entry. Duplicate output hashes are rejected because two
// distinct outputs claiming identical bytes would make the chain ambiguous.
func (t *Trace) Record(e Entry) error {
	if e.Output == "" || e.OutputHash == "" {
		return clock.New(clock.KindRecovery, clock.CodeHashMismatch, e.Output,
			"entry missing output identity")
	}
	key := e.Output + "@" + e.OutputHash
	if _, ok := t.seen[key]; ok {
		return clock.New(clock.KindRecovery, clock.CodeHashMismatch, e.Output,
			"duplicate provenance entry")
	}
	t.seen[key] = struct{}{}
	t.entries = append(t.entries, e)
	return nil
}

// Entries returns the recorded chain.
func (t *Trace) Entries() []Entry { return t.entries }

// Complete reports whether every entry has at least one input hash and a
// manifest link, i.e. the chain from output to playlist entry is unbroken.
func (t *Trace) Complete() bool {
	if len(t.entries) == 0 {
		return false
	}
	for _, e := range t.entries {
		if e.OutputHash == "" || e.ManifestURI == "" || len(e.InputHashes) == 0 {
			return false
		}
		for _, h := range e.InputHashes {
			if len(h) != 64 || !isHex(h) {
				return false
			}
		}
	}
	return true
}

// Resolve walks from an output hash back to the originating entries.
func (t *Trace) Resolve(outputHash string) ([]Entry, error) {
	var out []Entry
	for _, e := range t.entries {
		if e.OutputHash == outputHash || e.Output == outputHash {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil, clock.New(clock.KindRecovery, clock.CodeHashMismatch,
			outputHash, "output has no provenance entry")
	}
	return out, nil
}

// Lineage follows input hashes transitively back to manifest entries. It
// returns every distinct manifest URI reachable from outputHash.
func (t *Trace) Lineage(outputHash string) ([]string, error) {
	byOutput := map[string]Entry{}
	byInput := map[string]Entry{}
	for _, e := range t.entries {
		byOutput[e.OutputHash] = e
		for _, h := range e.InputHashes {
			byInput[h] = e
		}
	}
	visited := map[string]struct{}{}
	var uris []string
	queue := []string{outputHash}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		if _, ok := visited[h]; ok {
			continue
		}
		visited[h] = struct{}{}
		if e, ok := byOutput[h]; ok {
			if e.ManifestURI != "" {
				uris = append(uris, e.ManifestURI)
			}
			for _, in := range e.InputHashes {
				queue = append(queue, in)
			}
			continue
		}
		if e, ok := byInput[h]; ok {
			if e.ManifestURI != "" {
				uris = append(uris, e.ManifestURI)
			}
			queue = append(queue, e.InputHashes...)
		}
	}
	sort.Strings(uris)
	uniq := uris[:0]
	for i, u := range uris {
		if i == 0 || u != uris[i-1] {
			uniq = append(uniq, u)
		}
	}
	if len(uniq) == 0 {
		return nil, clock.New(clock.KindRecovery, clock.CodeHashMismatch,
			outputHash, "no manifest lineage")
	}
	return uniq, nil
}

// Summary is a stable digest of the whole chain, so two runs can be compared
// without comparing every entry.
func (t *Trace) Summary() string {
	b, _ := json.Marshal(t.entries)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// JSON renders the chain for the artifact written by the demo.
func (t *Trace) JSON() ([]byte, error) {
	return json.MarshalIndent(struct {
		Entries  []Entry `json:"entries"`
		Complete bool    `json:"complete"`
		Summary  string  `json:"summary"`
	}{t.entries, t.Complete(), t.Summary()}, "", "  ")
}

// Merge folds another trace's entries in, preserving order.
func (t *Trace) Merge(other *Trace) error {
	for _, e := range other.entries {
		if err := t.Record(e); err != nil {
			return err
		}
	}
	return nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// HashPayload returns the hex SHA-256 of a payload, the canonical input hash.
func HashPayload(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Describe renders an entry for terminal output.
func (e Entry) Describe() string {
	return fmt.Sprintf("%s <- %s (%d inputs, %d decisions)",
		e.Output, e.ManifestURI, len(e.InputHashes), len(e.Decisions))
}
