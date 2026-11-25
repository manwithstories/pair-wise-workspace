// Package fingerprint hashes canonical media payloads and tracks duplicates.
package fingerprint

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"sync"
)

// Source identifies where an identical canonical payload appeared.
type Source struct {
	ClipID string `json:"clip_id"`
	Source string `json:"source"`
}

// Chunk is one elementary payload. PTS is intentionally not hashed: moving the
// same media to another timeline offset must not change its content identity.
type Chunk struct {
	PTSTicks uint64
	Payload  []byte
}

// ClipSHA256 returns the 64-character hex SHA-256 of exact committed bytes.
func ClipSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Content computes the canonical media fingerprint. Callers pass only
// elementary payloads after TS/RTP and PES headers have been removed.
//
// Each chunk is length-framed so boundaries cannot be confused, but PTS and
// other timestamp-offset fields are deliberately absent.
func Content(chunks []Chunk) string {
	h := sha256.New()
	var length [8]byte
	for _, c := range chunks {
		binary.LittleEndian.PutUint64(length[:], uint64(len(c.Payload)))
		h.Write(length[:])
		h.Write(c.Payload)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ContentBytes is a convenience for one contiguous canonical payload.
func ContentBytes(payload []byte) string {
	return Content([]Chunk{{Payload: payload}})
}

// Record stores provenance for one fingerprint.
type Record struct {
	Fingerprint string   `json:"fingerprint"`
	Sources     []Source `json:"sources"`
	BlobPath    string   `json:"blob_path,omitempty"`
}

// DedupIndex records all sources while allowing only one blob write.
type DedupIndex struct {
	mu      sync.Mutex
	records map[string]*Record
}

func NewDedupIndex() *DedupIndex {
	return &DedupIndex{records: make(map[string]*Record)}
}

// Observe records a source. First returns true when this is the first sighting
// and the caller should write the blob; later sightings return false.
func (d *DedupIndex) Observe(fp string, src Source, blobPath string) (first bool, rec Record) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.records[fp]
	if !ok {
		r = &Record{Fingerprint: fp, BlobPath: blobPath}
		d.records[fp] = r
		first = true
	}
	for _, s := range r.Sources {
		if s == src {
			return false, *r
		}
	}
	r.Sources = append(r.Sources, src)
	return first, *r
}

// Record returns a sorted copy suitable for manifests/output.
func (d *DedupIndex) Record(fp string) (Record, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.records[fp]
	if !ok {
		return Record{}, false
	}
	out := *r
	out.Sources = append([]Source(nil), r.Sources...)
	sort.Slice(out.Sources, func(i, j int) bool {
		if out.Sources[i].ClipID != out.Sources[j].ClipID {
			return out.Sources[i].ClipID < out.Sources[j].ClipID
		}
		return out.Sources[i].Source < out.Sources[j].Source
	})
	return out, true
}

// Records returns a deterministic snapshot ordered by fingerprint.
func (d *DedupIndex) Records() []Record {
	d.mu.Lock()
	fps := make([]string, 0, len(d.records))
	for fp := range d.records {
		fps = append(fps, fp)
	}
	d.mu.Unlock()
	sort.Strings(fps)
	out := make([]Record, 0, len(fps))
	for _, fp := range fps {
		if r, ok := d.Record(fp); ok {
			out = append(out, r)
		}
	}
	return out
}
