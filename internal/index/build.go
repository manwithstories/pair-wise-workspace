// Package index builds a random-access index from repaired keyframes.  Every
// entry points at a real block boundary that was observed during EBML parsing;
// the index is never reconstructed from nominal bitrates.  Repeated builds
// over the same input produce the same digest.
package index

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/abema/go-mp4"

	"archivecheck/internal/clock"
	"archivecheck/internal/fault"
	"archivecheck/internal/timeline"
)

// Entry is one random-access point.
type Entry struct {
	Track    uint64 `json:"track"`
	TimeUsec int64  `json:"time_us"`
	Time     string `json:"time"`
	// Offset is the header offset of the block element, verified to carry the
	// track's ElementID and the expected payload length.
	Offset     int64  `json:"offset"`
	PayloadLen int64  `json:"payload_len"`
	PayloadOff int64  `json:"payload_offset"`
	Keyframe   bool   `json:"keyframe"`
	Digest     uint32 `json:"payload_crc32"`
	Element    string `json:"element"`
}

// Index is the serialized random-access structure.
type Index struct {
	Entries  []Entry        `json:"entries"`
	PerTrack map[string]int `json:"entries_per_track"`
	Digest   string         `json:"digest"`
	Faults   []faultEntry   `json:"-"`
}

type faultEntry struct {
	Track  uint64 `json:"track"`
	Offset int64  `json:"offset"`
	Reason string `json:"reason"`
}

// BlockRef is what the index builder needs to verify an entry against the
// source: the element id and its recorded header offset.
type BlockRef struct {
	Track      uint64
	Offset     int64
	PayloadOff int64
	PayloadLen int64
	Element    string
	Keyframe   bool
	Digest     uint32
	Time       clock.Rational
}

// Verifier checks that a candidate offset really starts a block element.
type Verifier interface {
	// VerifyBlock returns the block element parsed at offset, or an error when
	// the offset is not a block boundary.
	VerifyBlock(off int64) (BlockRef, error)
}

// BuildConfig selects which tracks are indexed and how many entries.
type BuildConfig struct {
	// KeyframesOnly indexes only random-access points (the archive default).
	KeyframesOnly bool
	// MaxEntriesPerTrack caps index size per track; 0 means unlimited.
	MaxEntriesPerTrack int
	// SampleEvery keeps every Nth keyframe; 0 or 1 keeps all.
	SampleEvery int
}

// Build produces the index from repaired tracks.  v verifies every offset
// against the source; an entry failing verification is reported as a fault
// rather than emitted.
func Build(tracks map[uint64]*timeline.TrackInfo, order []uint64, cfg BuildConfig, v Verifier, faults *fault.Set) (*Index, error) {
	idx := &Index{PerTrack: make(map[string]int)}
	if cfg.SampleEvery < 1 {
		cfg.SampleEvery = 1
	}
	for _, trackNum := range order {
		info := tracks[trackNum]
		if info == nil {
			continue
		}
		kept := 0
		seen := 0
		for _, f := range info.Frames {
			if f.Missing {
				continue
			}
			if cfg.KeyframesOnly && !f.Keyframe {
				continue
			}
			seen++
			if (seen-1)%cfg.SampleEvery != 0 {
				continue
			}
			if cfg.MaxEntriesPerTrack > 0 && kept >= cfg.MaxEntriesPerTrack {
				break
			}
			ref, err := v.VerifyBlock(f.Offset)
			if err != nil {
				if faults != nil {
					faults.Add(fault.New(fault.ModuleIndex, fault.KindIndexMisaligned, fault.NewOffset(uint64(f.Offset)),
						fmt.Sprintf("track %d candidate offset is not a block boundary: %v", trackNum, err)).OnTrack(trackNum))
				}
				continue
			}
			if ref.Track != trackNum {
				if faults != nil {
					faults.Add(fault.New(fault.ModuleIndex, fault.KindIndexMisaligned, fault.NewOffset(uint64(f.Offset)),
						fmt.Sprintf("offset belongs to track %d, expected %d", ref.Track, trackNum)).OnTrack(trackNum))
				}
				continue
			}
			idx.Entries = append(idx.Entries, Entry{
				Track:      trackNum,
				TimeUsec:   int64(f.Repaired.Float64() * 1e6),
				Time:       f.Repaired.String(),
				Offset:     ref.Offset,
				PayloadLen: ref.PayloadLen,
				PayloadOff: ref.PayloadOff,
				Keyframe:   ref.Keyframe || f.Keyframe,
				Digest:     ref.Digest,
				Element:    ref.Element,
			})
			kept++
		}
		idx.PerTrack[fmt.Sprintf("%d", trackNum)] = kept
	}
	sort.SliceStable(idx.Entries, func(i, j int) bool {
		a, b := idx.Entries[i], idx.Entries[j]
		if a.TimeUsec != b.TimeUsec {
			return a.TimeUsec < b.TimeUsec
		}
		if a.Track != b.Track {
			return a.Track < b.Track
		}
		return a.Offset < b.Offset
	})
	idx.Digest = digestOf(idx.Entries)
	return idx, nil
}

func digestOf(entries []Entry) string {
	h := sha256.New()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(entries)
	h.Write(buf.Bytes())
	return hex.EncodeToString(h.Sum(nil))
}

// Digest recomputes the digest of the current entries.
func (ix *Index) DigestOf() string { return digestOf(ix.Entries) }

// WriteJSON serializes the index deterministically (sorted keys via struct
// order, no map iteration in the document).
func (ix *Index) WriteJSON(w io.Writer) error {
	type doc struct {
		Version  int            `json:"version"`
		Entries  []Entry        `json:"entries"`
		PerTrack map[string]int `json:"entries_per_track"`
		Digest   string         `json:"digest"`
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc{Version: 1, Entries: ix.Entries, PerTrack: ix.PerTrack, Digest: ix.Digest})
}

// Seek returns the last entry at or before t on a track, giving random
// access to the containing GOP.
func (ix *Index) Seek(track uint64, t clock.Rational) (Entry, bool) {
	usec := int64(t.Float64() * 1e6)
	var best Entry
	found := false
	for _, e := range ix.Entries {
		if e.Track != track || e.TimeUsec > usec {
			continue
		}
		if !found || e.TimeUsec > best.TimeUsec {
			best = e
			found = true
		}
	}
	return best, found
}

// Sidecar writes a minimal ISOBMFF structure containing an ftyp brand and a
// free box carrying the index digest.  go-mp4 performs the box marshaling so
// the sidecar is a structurally valid MP4 fragment usable by other tools.
type Sidecar struct {
	MajorBrand       [4]byte
	MinorVersion     uint32
	CompatibleBrands [][4]byte
	Digest           string
	Entries          int
}

// NewSidecar returns a sidecar describing the index.
func NewSidecar(ix *Index) Sidecar {
	return Sidecar{
		MajorBrand:       [4]byte{'a', 'r', 'c', 'h'},
		MinorVersion:     1,
		CompatibleBrands: [][4]byte{{'i', 's', 'o', '6'}, {'m', 'p', '4', '2'}},
		Digest:           ix.Digest,
		Entries:          len(ix.Entries),
	}
}

// Write serializes the sidecar as ftyp + free boxes.  mp4.Marshal emits box
// payloads only, so each common header is encoded explicitly and its size is
// derived from the marshaled payload.
func (s Sidecar) Write(w io.Writer) error {
	ftyp := &mp4.Ftyp{MajorBrand: s.MajorBrand, MinorVersion: s.MinorVersion}
	for _, b := range s.CompatibleBrands {
		ftyp.AddCompatibleBrand(b)
	}
	note := []byte(fmt.Sprintf("archivecheck-index v1 entries=%d sha256=%s", s.Entries, s.Digest))
	boxes := []struct {
		box mp4.IImmutableBox
	}{
		{ftyp},
		{&mp4.Free{Data: note}},
	}
	for _, b := range boxes {
		var payload bytes.Buffer
		if _, err := mp4.Marshal(&payload, b.box, mp4.Context{}); err != nil {
			return fmt.Errorf("index: marshal %s: %w", b.box.GetType().String(), err)
		}
		info := &mp4.BoxInfo{
			Size:       uint64(mp4.SmallHeaderSize + payload.Len()),
			HeaderSize: mp4.SmallHeaderSize,
			Type:       b.box.GetType(),
		}
		if _, err := w.Write(mp4.EncodeBoxInfo(info)); err != nil {
			return err
		}
		if _, err := w.Write(payload.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

// ParseSidecar reads back the ftyp and free boxes written by Write, proving
// the sidecar is valid ISOBMFF understood by go-mp4.
func ParseSidecar(r io.ReadSeeker) (Sidecar, string, error) {
	var out Sidecar
	var note string
	var ftyp *mp4.Ftyp
	var free *mp4.Free
	infos, err := mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case mp4.BoxTypeFtyp():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			ftyp = box.(*mp4.Ftyp)
		case mp4.BoxTypeFree():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			free = box.(*mp4.Free)
		}
		return nil, nil
	})
	if err != nil {
		return Sidecar{}, "", fmt.Errorf("index: parse sidecar: %w", err)
	}
	if len(infos) == 0 || ftyp == nil {
		return Sidecar{}, "", fmt.Errorf("index: sidecar has no ftyp box")
	}
	out.MajorBrand = ftyp.MajorBrand
	out.MinorVersion = ftyp.MinorVersion
	for _, b := range ftyp.CompatibleBrands {
		out.CompatibleBrands = append(out.CompatibleBrands, b.CompatibleBrand)
	}
	if free != nil {
		note = string(free.Data)
	}
	return out, note, nil
}
