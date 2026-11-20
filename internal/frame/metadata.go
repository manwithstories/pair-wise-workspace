package frame

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/mediamend/mediamend/internal/fault"
)

// MetadataFileName is the sidecar table holding per-record SHA-256 digests.
// It is JSONL so a damaged table can still be read up to the bad line.
const MetadataFileName = "frames.meta"

// MetaEntry ties one record to its payload digest and source location.
type MetaEntry struct {
	Camera    string `json:"camera_id"`
	Seq       uint64 `json:"seq"`
	DeviceTS  uint64 `json:"device_ts"`
	Length    uint32 `json:"body_length"`
	Offset    int64  `json:"offset"`
	SHA256    string `json:"sha256"`
	Keyframe  bool   `json:"keyframe"`
	CapturedA int64  `json:"captured_at_unix_nano"`
}

// metadataWire is the on-disk JSON line. Field names are stable.
type metadataWire struct {
	Camera   string `json:"camera_id"`
	Seq      uint64 `json:"seq"`
	DeviceTS uint64 `json:"device_ts"`
	Length   uint32 `json:"body_length"`
	Offset   int64  `json:"offset"`
	SHA256   string `json:"sha256"`
	Keyframe bool   `json:"keyframe,omitempty"`
	Captured int64  `json:"captured_at_unix_nano,omitempty"`
}

// MetaRequest is one entry to persist; the payload is hashed while streaming
// and never retained by the writer.
type MetaRequest struct {
	Camera   string
	Seq      uint64
	DeviceTS uint64
	Length   uint32
	Offset   int64
	Keyframe bool
	Captured int64
}

// MetadataWriter streams entries to a sidecar file, flushing per record so a
// crash leaves a readable prefix rather than a partial line.
type MetadataWriter struct {
	path string
	f    *os.File
	buf  [sha256.Size * 2]byte
}

// CreateMetadata starts a fresh sidecar table at dir/name.
func CreateMetadata(dir, name string) (*MetadataWriter, error) {
	if name == "" {
		name = MetadataFileName
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &MetadataWriter{path: path, f: f}, nil
}

// Write hashes payload and appends one entry. The digest is computed in a
// single streaming pass over the caller's slice.
func (w *MetadataWriter) Write(req MetaRequest, payload []byte) (string, error) {
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	line := fmt.Sprintf(
		"{\"camera_id\":%q,\"seq\":%d,\"device_ts\":%d,\"body_length\":%d,\"offset\":%d,\"sha256\":%q",
		req.Camera, req.Seq, req.DeviceTS, req.Length, req.Offset, digest)
	if req.Keyframe {
		line += ",\"keyframe\":true"
	}
	if req.Captured != 0 {
		line += fmt.Sprintf(",\"captured_at_unix_nano\":%d", req.Captured)
	}
	line += "}\n"
	if _, err := io.WriteString(w.f, line); err != nil {
		return "", err
	}
	return digest, nil
}

// Close flushes and closes the sidecar.
func (w *MetadataWriter) Close() error {
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// Metadata is an in-memory lookup of the sidecar entries keyed by (camera,
// seq). Duplicate records overwrite with the last observation so the reader
// can detect disagreement.
type Metadata struct {
	byKey  map[string]MetaEntry
	list   []MetaEntry
	faults fault.List
}

func metaKey(camera string, seq uint64) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], seq)
	return camera + "\x00" + string(b[:])
}

// LoadMetadata reads a sidecar table. A malformed line yields a recoverable
// fault and is skipped; the rest of the table stays usable.
func LoadMetadata(path string) (*Metadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	md := &Metadata{byKey: map[string]MetaEntry{}}
	br := newLineScanner(f)
	n := 0
	for br.next() {
		n++
		var w metadataWire
		if err := br.unmarshal(&w); err != nil {
			md.faults = append(md.faults, fault.Fault{
				Kind: fault.KindMalformedMetadata, Stage: fault.StageParse,
				Offset: br.offset(), File: path,
				Message: fmt.Sprintf("malformed metadata line %d: %v", n, err),
			})
			continue
		}
		e := MetaEntry{
			Camera: w.Camera, Seq: w.Seq, DeviceTS: w.DeviceTS, Length: w.Length,
			Offset: w.Offset, SHA256: w.SHA256, Keyframe: w.Keyframe, CapturedA: w.Captured,
		}
		md.byKey[metaKey(e.Camera, e.Seq)] = e
		md.list = append(md.list, e)
	}
	if err := br.err(); err != nil {
		return md, err
	}
	return md, nil
}

// Lookup finds the metadata for a camera+sequence pair.
func (m *Metadata) Lookup(camera string, seq uint64) (MetaEntry, bool) {
	e, ok := m.byKey[metaKey(camera, seq)]
	return e, ok
}

// Entries returns a copy of every entry in file order.
func (m *Metadata) Entries() []MetaEntry { return append([]MetaEntry(nil), m.list...) }

// Len reports the number of loaded entries.
func (m *Metadata) Len() int { return len(m.list) }

// Faults returns metadata parse defects.
func (m *Metadata) Faults() fault.List { return m.faults }

// SortEntries orders entries by camera then sequence.
func SortEntries(es []MetaEntry) {
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].Camera != es[j].Camera {
			return es[i].Camera < es[j].Camera
		}
		return es[i].Seq < es[j].Seq
	})
}

// VerifyPayload recomputes the SHA-256 of payload and compares it with the
// stored digest, producing a fault with the frame's byte offset on mismatch.
func VerifyPayload(e MetaEntry, payload []byte, file string) (bool, fault.Fault) {
	sum := sha256.Sum256(payload)
	got := hex.EncodeToString(sum[:])
	if got == e.SHA256 {
		return true, fault.Fault{}
	}
	return false, fault.Fault{
		Kind: fault.KindChecksumMismatch, Stage: fault.StageParse, Camera: e.Camera,
		Seq: e.Seq, Offset: e.Offset, File: file,
		Message: "payload SHA-256 does not match metadata", Expected: e.SHA256, Actual: got,
	}
}
