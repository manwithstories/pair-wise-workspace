// Package audit builds the delivery manifest. It records input tracks, output
// mappings, time ranges, and per-track statistics, then re-verifies the
// written file's hashes before the pipeline is allowed to commit.
package audit

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"mediapipe/internal/mapping"
	"mediapipe/internal/media"
	"mediapipe/internal/remux"
	"mediapipe/internal/timeline"
)

// TrackRecord is one manifest track entry.
type TrackRecord struct {
	InID       uint64          `json:"in_id"`
	OutID      uint64          `json:"out_id"`
	Type       string          `json:"type"`
	Codec      string          `json:"codec"`
	Language   string          `json:"language"`
	Packets    int             `json:"packets"`
	Bytes      int64           `json:"bytes"`
	Dropped    bool            `json:"dropped,omitempty"`
	Start      media.Timestamp `json:"start_ns"`
	End        media.Timestamp `json:"end_ns"`
	Keyframes  int             `json:"keyframes"`
	PayloadCRC uint64          `json:"payload_crc"`
}

// Manifest is the recorded outcome of one job.
type Manifest struct {
	Version             int             `json:"version"`
	JobID               string          `json:"job_id"`
	Source              string          `json:"source"`
	Target              string          `json:"target"`
	TimestampBase       string          `json:"timestamp_base"`
	TimeStart           media.Timestamp `json:"time_start_ns"`
	TimeEnd             media.Timestamp `json:"time_end_ns"`
	Duration            media.Timestamp `json:"duration_ns"`
	TargetBPS           int64           `json:"target_bps"`
	ActualBPS           int64           `json:"actual_bps"`
	DeltaPct            float64         `json:"delta_pct"`
	PayloadHashMismatch int             `json:"payload_hash_mismatch"`
	CRCErrors           int             `json:"crc_errors"`
	ManifestSkew        media.Timestamp `json:"manifest_skew_ns"`
	BufferPeak          int64           `json:"buffer_peak_bytes"`
	ReplayDigest        string          `json:"replay_digest"`
	OutputDigest        string          `json:"output_digest"`
	OutputBytes         int64           `json:"output_bytes"`
	Tracks              []TrackRecord   `json:"tracks"`
	Windows             []WindowRecord  `json:"windows"`
	Watermark           media.Timestamp `json:"watermark_ns"`
	Quarantined         int             `json:"quarantined"`
	Committed           bool            `json:"committed"`
	Reason              string          `json:"reason,omitempty"`
	CreatedBy           string          `json:"created_by"`
}

// WindowRecord mirrors timeline.WindowSummary for the manifest.
type WindowRecord struct {
	Start      media.Timestamp `json:"start_ns"`
	End        media.Timestamp `json:"end_ns"`
	Events     int             `json:"events"`
	Bytes      int64           `json:"bytes"`
	Digest     string          `json:"digest"`
	Recomputed bool            `json:"recomputed,omitempty"`
}

// Builder accumulates manifest inputs.
type Builder struct {
	m Manifest
}

// NewBuilder starts a manifest for a job.
func NewBuilder(jobID, source string, target mapping.Target) *Builder {
	return &Builder{m: Manifest{
		Version:       1,
		JobID:         jobID,
		Source:        source,
		Target:        string(target),
		TimestampBase: "nanoseconds",
		CreatedBy:     "mediapipe",
	}}
}

// SetRate records the target and measured bitrates along with the deviation.
func (b *Builder) SetRate(targetBPS, actualBPS int64) {
	b.m.TargetBPS = targetBPS
	b.m.ActualBPS = actualBPS
	if targetBPS > 0 {
		b.m.DeltaPct = float64(actualBPS-targetBPS) / float64(targetBPS) * 100
	}
}

// SetOutput records the written file's digest and size.
func (b *Builder) SetOutput(sum remux.Summary) {
	b.m.OutputDigest = fmt.Sprintf("%x", sum.Digest[:])
	b.m.OutputBytes = sum.Bytes
	b.m.TimeStart = sum.TimeStart
	b.m.TimeEnd = sum.TimeEnd
	b.m.Duration = sum.TimeEnd - sum.TimeStart
	if b.m.Duration < 0 {
		b.m.Duration = 0
	}
}

// SetTimeline records watermark engine results.
func (b *Builder) SetTimeline(e *timeline.Engine, windows []timeline.WindowSummary, digest [32]byte) {
	b.m.Watermark = e.Watermark()
	b.m.ReplayDigest = timeline.HexDigest(digest)
	b.m.Quarantined = len(e.Quarantine())
	b.m.Windows = make([]WindowRecord, 0, len(windows))
	for _, w := range windows {
		b.m.Windows = append(b.m.Windows, WindowRecord{
			Start: w.Start, End: w.End, Events: w.Events, Bytes: w.Bytes,
			Digest: timeline.HexDigest(w.Digest), Recomputed: w.Recomputed,
		})
	}
}

// SetBufferPeak records the highest queue occupancy.
func (b *Builder) SetBufferPeak(peak int64) { b.m.BufferPeak = peak }

// SetVerification records the re-demux comparison.
func (b *Builder) SetVerification(mismatches, crcErrors int) {
	b.m.PayloadHashMismatch = mismatches
	b.m.CRCErrors = crcErrors
}

// SetSkew records the largest difference between the manifest's declared
// timeline and the packet timeline.
func (b *Builder) SetSkew(skew media.Timestamp) { b.m.ManifestSkew = skew }

// SetTracks records per-track statistics derived from packets.
func (b *Builder) SetTracks(plan *mapping.Plan, packets []media.Packet) {
	recs := make(map[uint64]*TrackRecord)
	for _, r := range plan.Kept {
		recs[r.OutTrack] = &TrackRecord{
			InID: r.In.ID, OutID: r.OutTrack, Type: r.OutType.String(),
			Codec: string(r.OutCodec), Language: r.OutLanguage,
		}
	}
	for _, r := range plan.Dropped {
		recs[r.OutTrack] = &TrackRecord{
			InID: r.In.ID, OutID: r.OutTrack, Type: r.OutType.String(),
			Codec: string(r.OutCodec), Language: r.OutLanguage, Dropped: true,
		}
	}
	for _, p := range packets {
		out, ok := plan.Remap(p.Track)
		if !ok {
			continue
		}
		rec, ok := recs[out]
		if !ok {
			continue
		}
		if rec.Packets == 0 {
			rec.Start = p.DTS
			rec.End = p.End()
		} else {
			if p.DTS < rec.Start {
				rec.Start = p.DTS
			}
			if p.End() > rec.End {
				rec.End = p.End()
			}
		}
		rec.Packets++
		rec.Bytes += int64(len(p.Data))
		if p.Keyframe {
			rec.Keyframes++
		}
		rec.PayloadCRC = combineCRC(rec.PayloadCRC, p.CRC, uint64(len(p.Data)))
	}
	ids := make([]uint64, 0, len(recs))
	for id := range recs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	b.m.Tracks = make([]TrackRecord, 0, len(ids))
	for _, id := range ids {
		b.m.Tracks = append(b.m.Tracks, *recs[id])
	}
}

// combineCRC folds each packet CRC into a stable per-track rolling value.
func combineCRC(acc uint64, crc uint32, size uint64) uint64 {
	h := sha256.New()
	var buf [12]byte
	putU64(buf[0:8], acc)
	putU32(buf[8:12], crc)
	h.Write(buf[:])
	var sz [8]byte
	putU64(sz[:], size)
	h.Write(sz[:])
	sum := h.Sum(nil)
	return uint64(sum[0])<<56 | uint64(sum[1])<<48 | uint64(sum[2])<<40 |
		uint64(sum[3])<<32 | uint64(sum[4])<<24 | uint64(sum[5])<<16 |
		uint64(sum[6])<<8 | uint64(sum[7])
}

func putU64(dst []byte, v uint64) {
	for i := 0; i < 8; i++ {
		dst[i] = byte(v >> uint(56-8*i))
	}
}

func putU32(dst []byte, v uint32) {
	for i := 0; i < 4; i++ {
		dst[i] = byte(v >> uint(24-8*i))
	}
}

// Commit marks the manifest as committed when every delivery gate passes.
// Failures leave the reason recorded and clear the commit flag.
func (b *Builder) Commit() error {
	var reasons []string
	if b.m.PayloadHashMismatch != 0 {
		reasons = append(reasons, fmt.Sprintf("payload hash mismatches: %d", b.m.PayloadHashMismatch))
	}
	if b.m.CRCErrors != 0 {
		reasons = append(reasons, fmt.Sprintf("crc errors: %d", b.m.CRCErrors))
	}
	if absTS(b.m.ManifestSkew) > media.Millisecond {
		reasons = append(reasons, fmt.Sprintf("manifest skew %d ns exceeds 1ms", b.m.ManifestSkew))
	}
	if b.m.Quarantined != 0 {
		reasons = append(reasons, fmt.Sprintf("quarantined events: %d", b.m.Quarantined))
	}
	if len(reasons) > 0 {
		b.m.Committed = false
		b.m.Reason = joinReasons(reasons)
		return media.NewError("audit.commit", media.ClassFatal, fmt.Errorf("%s", b.m.Reason))
	}
	b.m.Committed = true
	b.m.Reason = ""
	return nil
}

func joinReasons(r []string) string {
	out := ""
	for i, s := range r {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}

func absTS(t media.Timestamp) media.Timestamp {
	if t < 0 {
		return -t
	}
	return t
}

// Manifest returns the assembled value.
func (b *Builder) Manifest() Manifest { return b.m }

// Digest returns the SHA-256 of the canonical JSON manifest, used to compare
// resumed and uninterrupted runs.
func (b *Builder) Digest() ([32]byte, error) {
	data, err := json.Marshal(b.m)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(data), nil
}

// WriteFile writes the manifest as indented JSON.
func (b *Builder) WriteFile(path string) error {
	data, err := json.MarshalIndent(b.m, "", "  ")
	if err != nil {
		return media.NewError("audit.write", media.ClassInterrupted, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return media.NewError("audit.write", media.ClassInterrupted, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return media.NewError("audit.write", media.ClassInterrupted, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return media.NewError("audit.write", media.ClassInterrupted, err)
	}
	return nil
}

// ReadFile loads a manifest, for resume comparisons and tests.
func ReadFile(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, media.NewError("audit.read", media.ClassInterrupted, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, media.NewError("audit.read", media.ClassFatal, err)
	}
	return &m, nil
}

// HashBytes returns the SHA-256 of a byte slice as a hex string.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:])
}

// PackageHash hashes the output file plus the manifest's identifying fields so
// a delivery can compare packages without shipping both files.
func PackageHash(output []byte, m *Manifest) string {
	h := sha256.New()
	h.Write(output)
	h.Write([]byte(m.JobID))
	h.Write([]byte(m.Target))
	h.Write([]byte(m.ReplayDigest))
	for _, t := range m.Tracks {
		fmt.Fprintf(h, "|%d:%d:%s:%s", t.InID, t.OutID, t.Codec, t.Language)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
