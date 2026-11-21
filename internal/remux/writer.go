package remux

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math"

	"mkvarchive/internal/ebml"
	"mkvarchive/internal/model"
	"mkvarchive/internal/timeline"
	"mkvarchive/internal/track"
)

// BlockEnvelope is one block presented to the writer in source stream order.
// Payload is the full Matroska block bytes (track VINT + timecode + flags +
// frame) and must be archived without altering the frame portion.
type BlockEnvelope struct {
	SourceTrackNumber uint64
	ElementID         uint32
	ClusterTimecode   int64
	RawTimecode       int64
	HeaderOffset      int64
	Payload           []byte
}

// Writer streams an archival Matroska: unknown-length segment and clusters
// keep memory bounded, and every frame payload passes through unchanged.
type Writer struct {
	w              io.Writer
	plan           *Plan
	bySourceNumber map[uint64]track.Mapping
	clock          timeline.Clock
	builder        *timeline.Builder

	clusterOpen bool
	clusterTC   int64
	bytesOut    int64
	digests     []PayloadDigest
	frameHashes map[string]bool
}

// NewWriter emits the EBML header, segment opening and metadata sections.
// infoData and tracksData are the verbatim source metadata; tracksData is
// renumbered so block track VINTs can be rewritten to archive track ids.
func NewWriter(w io.Writer, plan *Plan, infoData, tracksData []byte,
	manifestBuilder *timeline.Builder) (*Writer, error) {
	renumbered, err := track.RenumberTracks(tracksData, plan.Mappings)
	if err != nil {
		return nil, err
	}
	ww := &Writer{
		w:              w,
		plan:           plan,
		bySourceNumber: track.LookupBySourceNumber(plan.Mappings),
		clock:          timeline.NewClock(plan.TimestampScale),
		builder:        manifestBuilder,
		frameHashes:    map[string]bool{},
	}
	if err = ww.writeRaw(ebml.EncodeElement(model.IDEBMLHeader, defaultEBMLHeader())); err != nil {
		return nil, err
	}
	if err = ww.writeRaw(ebml.EncodeElementUnknown(model.IDSegment, nil, 8)); err != nil {
		return nil, err
	}
	if len(infoData) > 0 {
		if err = ww.writeRaw(ebml.EncodeElement(model.IDSegmentInfo, infoData)); err != nil {
			return nil, err
		}
	}
	if err = ww.writeRaw(ebml.EncodeElement(model.IDTracks, renumbered)); err != nil {
		return nil, err
	}
	return ww, nil
}

// WriteBlock archives one block, opening a new cluster whenever the cluster
// timecode changes. The frame payload is copied byte-for-byte; only the
// track-number VINT prefix is rewritten to the archive track id.
func (w *Writer) WriteBlock(b BlockEnvelope) error {
	// Envelope carries the on-wire VINT data value (track number minus one);
	// mappings are keyed by the declared Matroska track number.
	declaredNumber := b.SourceTrackNumber + 1
	m, ok := w.bySourceNumber[declaredNumber]
	if !ok {
		return model.NewError("TRACK_MAPPING_FAILED",
			"block references a track absent from the plan", b.HeaderOffset)
	}
	if !w.clusterOpen || w.clusterTC != b.ClusterTimecode {
		if err := w.closeCluster(); err != nil {
			return err
		}
		if err := w.openCluster(b.ClusterTimecode); err != nil {
			return err
		}
	}
	frame, err := frameOf(b.Payload)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(frame)
	hash := hex.EncodeToString(sum[:])
	w.frameHashes[hash] = true
	w.digests = append(w.digests, PayloadDigest{
		SourceTrackNumber: declaredNumber,
		ArchiveTrackID:    m.ArchiveID,
		SourceOffset:      b.HeaderOffset,
		RawTimecode:       b.RawTimecode,
		SHA256:            hash,
		Bytes:             len(frame),
	})
	w.builder.Observe(m.SourceUID, m.ArchiveID, m.Kind, b.RawTimecode)

	rewritten, err := rewriteTrackVINT(b.Payload, m.ArchiveID)
	if err != nil {
		return err
	}
	return w.writeRaw(ebml.EncodeElement(b.ElementID, rewritten))
}

// Close finalizes any open cluster. Unknown-length masters need no footer.
func (w *Writer) Close() error { return w.closeCluster() }

// QualityReport computes bitrate and alignment evidence. expectedDigests is
// the independently computed source-side digest list; every archived frame
// hash must be present there for PayloadSHA256Match to be true.
func (w *Writer) QualityReport(targetBitrateKbps, durationNS int64,
	expected map[string]bool, maxAlignmentMS float64) QualityReport {
	payloadBytes := 0
	for _, d := range w.digests {
		payloadBytes += d.Bytes
	}
	durationSec := float64(durationNS) / 1e9
	if durationSec <= 0 {
		durationSec = 1
	}
	actualKbps := float64(payloadBytes) * 8 / 1000 / durationSec
	deviation := 0.0
	if targetBitrateKbps > 0 {
		deviation = math.Abs(actualKbps-float64(targetBitrateKbps)) /
			float64(targetBitrateKbps) * 100
	}
	match := true
	for hash := range w.frameHashes {
		if !expected[hash] {
			match = false
			break
		}
	}
	return QualityReport{
		TargetBitrateKbps:   float64(targetBitrateKbps),
		ActualBitrateKbps:   round1(actualKbps),
		BitrateDeviationPct: round2(deviation),
		PayloadSHA256Match:  match,
		QualityBlocks:       len(w.digests),
		ManifestAlignmentMS: maxAlignmentMS,
		Digests:             w.digests,
	}
}

// Digests returns the archived payload digests in write order.
func (w *Writer) Digests() []PayloadDigest { return w.digests }

func (w *Writer) openCluster(tc int64) error {
	w.clusterOpen = true
	w.clusterTC = tc
	if err := w.writeRaw(ebml.EncodeElementUnknown(model.IDCluster, nil, 8)); err != nil {
		return err
	}
	return w.writeRaw(ebml.EncodeElement(model.IDClusterTimecode,
		ebml.EncodeUnsigned(uint64(tc))))
}

func (w *Writer) closeCluster() error {
	w.clusterOpen = false
	return nil
}

func (w *Writer) writeRaw(p []byte) error {
	n, err := w.w.Write(p)
	if err != nil {
		return err
	}
	w.bytesOut += int64(n)
	return nil
}

// FrameOf strips the block header (track VINT + 2-byte timecode + flags)
// and returns the verbatim compressed frame payload.
func FrameOf(payload []byte) ([]byte, error) {
	return frameOf(payload)
}

// frameOf strips the block header (track VINT + 2-byte timecode + flags).
func frameOf(payload []byte) ([]byte, error) {
	v, err := ebml.ReadVINT(bytes.NewReader(payload), 0)
	if err != nil {
		return nil, err
	}
	headerLen := v.Width + 3
	if len(payload) < headerLen {
		return nil, model.NewError("EBML_BLOCK_TRUNCATED",
			"block shorter than its fixed header", 0)
	}
	return payload[headerLen:], nil
}

// rewriteTrackVINT replaces only the leading track-number VINT, preserving
// timecode, flags and the frame payload verbatim.
func rewriteTrackVINT(payload []byte, archiveID uint64) ([]byte, error) {
	v, err := ebml.ReadVINT(bytes.NewReader(payload), 0)
	if err != nil {
		return nil, err
	}
	// On the wire the track VINT data value is track number minus one.
	newVINT, err := ebml.EncodeVINT(archiveID - 1)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(newVINT)+len(payload)-v.Width)
	out = append(out, newVINT...)
	out = append(out, payload[v.Width:]...)
	return out, nil
}

// defaultEBMLHeader emits a minimal, fixed EBML header for the archive copy.
func defaultEBMLHeader() []byte {
	var b bytes.Buffer
	b.Write(ebml.EncodeElement(0x4286, []byte{1})) // EBMLVersion
	b.Write(ebml.EncodeElement(0x42F7, []byte{1})) // EBMLReadVersion
	b.Write(ebml.EncodeElement(0x42F2, []byte{4})) // EBMLMaxIDLength
	b.Write(ebml.EncodeElement(0x42F3, []byte{8})) // EBMLMaxSizeLength
	b.Write(ebml.EncodeElement(0x4282, []byte("matroska")))
	b.Write(ebml.EncodeElement(0x4287, []byte{4}))
	b.Write(ebml.EncodeElement(0x4285, []byte{2}))
	return b.Bytes()
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }
