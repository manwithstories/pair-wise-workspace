// Package fingerprint turns normalized media windows into stable content
// hashes and collapses repeated windows onto the first produced result.
// The hash is a pure function of the normalized bytes, so reruns produce
// identical fingerprints regardless of source container metadata.
package fingerprint

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"

	"medpipe/internal/clock"
)

// FrameGeometry fixes the normalized video representation so different
// resolutions and pixel formats hash identically for identical content.
type FrameGeometry struct {
	Width  int
	Height int
	// FramesPerWindow is the fixed number of gray frames sampled per window.
	FramesPerWindow int
}

// DefaultGeometry is the compact canonical video size.
var DefaultGeometry = FrameGeometry{Width: 32, Height: 18, FramesPerWindow: 4}

// CanonicalWindow is the fully normalized representation of one segment
// window: mono PCM at a fixed rate plus fixed-geometry grayscale frames.
type CanonicalWindow struct {
	// SegmentID is the pipeline-local id of the window being fingerprinted.
	SegmentID string
	// AudioRate is always the canonical analysis rate (48 kHz).
	AudioRate int
	// MonoPCM holds the normalized mono s16le samples for the window.
	MonoPCM []int16
	// Geometry describes every entry in GrayFrames.
	Geometry FrameGeometry
	// GrayFrames are raw 8-bit grayscale bitmaps, each W*H bytes.
	GrayFrames [][]byte
	// ColorTags records source color metadata so differing tags are never
	// silently treated as identical content.
	ColorTags [4]uint16
}

// EncodingVersion lets the canonical format evolve without silently reusing
// hashes computed under an older layout.
const EncodingVersion uint16 = 1

// Fingerprint returns a 64-char lowercase hex SHA-256 over a deterministic
// length-prefixed binary encoding of the normalized window.
func Fingerprint(w CanonicalWindow) (string, error) {
	if w.AudioRate <= 0 {
		return "", clock.E(clock.KindArgument, "fingerprint",
			clock.WithDetail("missing canonical audio rate"))
	}
	if err := validateGeometry(w); err != nil {
		return "", err
	}
	h := sha256.New()
	writeU16(h, EncodingVersion)
	writeU32(h, uint32(w.AudioRate))
	writeU32(h, uint32(len(w.MonoPCM)))
	var pcm [2]byte
	for _, s := range w.MonoPCM {
		binary.LittleEndian.PutUint16(pcm[:], uint16(s))
		h.Write(pcm[:])
	}
	writeU16(h, uint16(w.Geometry.Width))
	writeU16(h, uint16(w.Geometry.Height))
	writeU16(h, uint16(w.Geometry.FramesPerWindow))
	writeU32(h, uint32(len(w.GrayFrames)))
	frameSize := w.Geometry.Width * w.Geometry.Height
	for _, frame := range w.GrayFrames {
		if len(frame) != frameSize {
			return "", clock.E(clock.KindArgument, "fingerprint",
				clock.WithDetail("frame %d bytes, want %d", len(frame), frameSize))
		}
		h.Write(frame)
	}
	for _, tag := range w.ColorTags {
		writeU16(h, tag)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func validateGeometry(w CanonicalWindow) error {
	g := w.Geometry
	if g.Width <= 0 || g.Height <= 0 || g.FramesPerWindow <= 0 {
		return clock.E(clock.KindArgument, "fingerprint",
			clock.WithDetail("invalid geometry: %+v", g))
	}
	if len(w.GrayFrames) > g.FramesPerWindow {
		return clock.E(clock.KindArgument, "fingerprint",
			clock.WithDetail("frames %d exceed per-window %d",
				len(w.GrayFrames), g.FramesPerWindow))
	}
	return nil
}

func writeU16(h interface{ Write([]byte) (int, error) }, v uint16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	h.Write(b[:])
}

func writeU32(h interface{ Write([]byte) (int, error) }, v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	h.Write(b[:])
}

// Decision tells the caller whether a window is new or a repeat.
type Decision struct {
	SegmentID   string
	Fingerprint string
	// CanonicalID is SegmentID for first-seen content, otherwise the id of
	// the first window that produced the same fingerprint.
	CanonicalID string
	Duplicate   bool
}

// Deduper records the first segment id for every fingerprint. It is safe for
// sequential pipeline use and yields stable references across reruns.
type Deduper struct {
	first map[string]string
	order []string
	hits  int
}

// NewDeduper builds an empty deduper.
func NewDeduper() *Deduper {
	return &Deduper{first: make(map[string]string)}
}

// Add registers a window and returns its dedup decision. Repeated windows
// reference the first result instead of creating a new output.
func (d *Deduper) Add(w CanonicalWindow) (Decision, error) {
	fp, err := Fingerprint(w)
	if err != nil {
		return Decision{}, err
	}
	if canonical, ok := d.first[fp]; ok {
		d.hits++
		return Decision{
			SegmentID:   w.SegmentID,
			Fingerprint: fp,
			CanonicalID: canonical,
			Duplicate:   true,
		}, nil
	}
	d.first[fp] = w.SegmentID
	d.order = append(d.order, fp)
	return Decision{
		SegmentID:   w.SegmentID,
		Fingerprint: fp,
		CanonicalID: w.SegmentID,
		Duplicate:   false,
	}, nil
}

// Hits reports how many repeated windows were collapsed.
func (d *Deduper) Hits() int { return d.hits }

// Fingerprints returns the distinct hashes in first-seen order.
func (d *Deduper) Fingerprints() []string {
	return append([]string(nil), d.order...)
}
