// Package verify independently re-parses produced MP4 segments and hashes
// them. It never trusts the engine's in-memory state: every claim is checked
// against bytes on disk.
package verify

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	mp4 "github.com/abema/go-mp4"

	"github.com/fieldcut/fieldcut/internal/engine"
	"github.com/fieldcut/fieldcut/internal/ingest"
)

// ParsedSegment is the re-parsed view of one fragment file.
type ParsedSegment struct {
	Path        string
	Rendition   string
	Index       int
	BaseDecode  uint64
	SampleCount int
	SampleSizes []uint32
	SampleFlags []uint32
	SampleData  [][]byte
	Hash        string
}

// KeyframeAligned reports that every sample's sync flag in the container
// matches its access-unit keyframe state. Normal fragments must additionally
// begin on a keyframe; a recovery fragment following fault resynchronization
// is exempt from that first-unit requirement.
func (p *ParsedSegment) KeyframeAligned(units []*ingest.AccessUnit, recovery bool) bool {
	if p.SampleCount == 0 || len(units) == 0 {
		return false
	}
	if !recovery && !units[0].Keyframe {
		return false
	}
	for i, u := range units {
		wantSync := u.Keyframe
		gotSync := p.SampleFlags[i]&0x02000000 != 0
		if wantSync != gotSync {
			return false
		}
	}
	return true
}

// ParseSegment reads a fragment file from scratch and reconstructs its
// sample table and mdat payload boundaries.
func ParseSegment(path string) (*ParsedSegment, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	parsed := &ParsedSegment{Path: path, Hash: hex.EncodeToString(sum[:])}

	r := bytes.NewReader(raw)
	var trun *mp4.Trun
	var tfdt *mp4.Tfdt
	handler := func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case mp4.BoxTypeTrun():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			trun = box.(*mp4.Trun)
		case mp4.BoxTypeTfdt():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			tfdt = box.(*mp4.Tfdt)
		case mp4.BoxTypeMdat():
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
		}
		return h.Expand()
	}
	if _, err := mp4.ReadBoxStructure(r, handler); err != nil {
		return nil, fmt.Errorf("reparse %s: %w", path, err)
	}
	if trun == nil || tfdt == nil {
		return nil, fmt.Errorf("reparse %s: missing trun/tfdt", path)
	}
	parsed.BaseDecode = tfdt.GetBaseMediaDecodeTime()
	parsed.SampleCount = int(trun.SampleCount)
	for _, e := range trun.Entries {
		parsed.SampleSizes = append(parsed.SampleSizes, e.SampleSize)
		parsed.SampleFlags = append(parsed.SampleFlags, e.SampleFlags)
	}

	// Re-read mdat and split by the declared sample sizes to prove payload
	// round-trips exactly, not merely that box headers parse.
	mdat, err := extractMdat(raw)
	if err != nil {
		return nil, err
	}
	off := 0
	for _, size := range parsed.SampleSizes {
		n := int(size)
		if off+n > len(mdat) {
			return nil, fmt.Errorf("reparse %s: sample overruns mdat", path)
		}
		parsed.SampleData = append(parsed.SampleData, append([]byte(nil), mdat[off:off+n]...))
		off += n
	}
	if off != len(mdat) {
		return nil, fmt.Errorf("reparse %s: %d trailing mdat bytes", path, len(mdat)-off)
	}
	return parsed, nil
}

// extractMdat locates the mdat box by a forward box scan and returns its
// payload. A manual scan keeps the verifier independent of the writer path.
func extractMdat(raw []byte) ([]byte, error) {
	pos := 0
	for pos+8 <= len(raw) {
		size := int(uint32(raw[pos])<<24 | uint32(raw[pos+1])<<16 |
			uint32(raw[pos+2])<<8 | uint32(raw[pos+3]))
		typ := string(raw[pos+4 : pos+8])
		hdr := 8
		if size == 1 {
			return nil, fmt.Errorf("largesize mdat not supported by fixture")
		}
		if size == 0 {
			size = len(raw) - pos
		}
		if typ == "mdat" {
			return raw[pos+hdr : pos+size], nil
		}
		pos += size
	}
	return nil, fmt.Errorf("no mdat box found")
}

// RoundtripReport aggregates the independent verification outcome.
type RoundtripReport struct {
	SegmentsChecked int
	SegmentsMatched int
	HashesMatched   int
	KeyframeOK      int
	KeyframeTotal   int
	// Mismatches carries rendition and shared-clock position for each
	// failure, never just a generic count.
	Mismatches []Mismatch
}

// Mismatch pinpoints a failed check.
type Mismatch struct {
	Rendition string
	PTS       uint64
	Reason    string
}

// Roundtrip re-parses every committed segment and compares it against both
// the checkpoint hash and the engine's expected access units.
func Roundtrip(segments []*engine.Segment) (*RoundtripReport, error) {
	rep := &RoundtripReport{KeyframeTotal: len(segments)}
	for _, seg := range segments {
		parsed, err := ParseSegment(seg.Path)
		if err != nil {
			return nil, err
		}
		rep.SegmentsChecked++
		if parsed.SampleCount == len(seg.Units) && payloadsEqual(parsed, seg) {
			rep.SegmentsMatched++
		} else {
			rep.Mismatches = append(rep.Mismatches, Mismatch{
				Rendition: seg.Rendition, PTS: seg.StartPTS, Reason: "sample roundtrip mismatch",
			})
		}
		wantHash := hex.EncodeToString(seg.Hash)
		if parsed.Hash == wantHash {
			rep.HashesMatched++
		} else {
			rep.Mismatches = append(rep.Mismatches, Mismatch{
				Rendition: seg.Rendition, PTS: seg.StartPTS, Reason: "hash mismatch",
			})
		}
		if parsed.KeyframeAligned(seg.Units, seg.Recovery) {
			rep.KeyframeOK++
		} else {
			rep.Mismatches = append(rep.Mismatches, Mismatch{
				Rendition: seg.Rendition, PTS: seg.StartPTS, Reason: "keyframe boundary mismatch",
			})
		}
	}
	return rep, nil
}

func payloadsEqual(p *ParsedSegment, seg *engine.Segment) bool {
	if len(p.SampleData) != len(seg.Units) {
		return false
	}
	for i, u := range seg.Units {
		if !bytes.Equal(p.SampleData[i], u.Payload) {
			return false
		}
		if p.SampleSizes[i] != uint32(len(u.Payload)) {
			return false
		}
	}
	return true
}

// Rates returns the threshold-scored ratios.
func (r *RoundtripReport) Rates() (roundtrip, hash, keyframe float64) {
	if r.SegmentsChecked == 0 {
		return 1, 1, 1
	}
	roundtrip = float64(r.SegmentsMatched) / float64(r.SegmentsChecked)
	hash = float64(r.HashesMatched) / float64(r.SegmentsChecked)
	keyframe = float64(r.KeyframeOK) / float64(r.KeyframeTotal)
	return
}
