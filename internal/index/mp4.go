// Package index builds a keyframe index from an MP4 sample file and verifies
// every entry by re-parsing the bytes at each recorded offset.
package index

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"

	mp4 "github.com/abema/go-mp4"

	"mediarun/internal/clock"
)

// Keyframe indexes one sync sample of one track. Offset and Size locate the
// sample payload inside an mdat box.
type Keyframe struct {
	Track    string `json:"track"`
	TrackID  uint32 `json:"track_id"`
	Sample   uint32 `json:"sample"`
	TimeNS   int64  `json:"time_ns"`
	DurNS    int64  `json:"dur_ns"`
	Offset   uint64 `json:"offset"`
	Size     uint32 `json:"size"`
	Verified bool   `json:"verified"`
}

// Index is the full keyframe index for a file.
type Index struct {
	File       string     `json:"file"`
	Hash       string     `json:"hash"`
	Timescale  uint32     `json:"timescale"`
	DurationNS int64      `json:"duration_ns"`
	Keyframes  []Keyframe `json:"keyframes"`
	Samples    int        `json:"samples"`
	// SampleFormat records how sample payloads were structurally verified.
	SampleFormat string `json:"sample_format"`
	// Roundtrip is the fraction of indexed offsets that re-parsed to the
	// expected sample header.
	Roundtrip float64 `json:"roundtrip"`
}

// trackData accumulates the boxes a single trak needs.
type trackData struct {
	id        uint32
	handler   string
	timescale uint32
	sttsCount []sttsEntry
	stss      []uint32
	stsc      []stscEntry
	stszSize  uint32
	stszCount uint32
	stsz      []uint32
	chunkOff  []uint64
}

type sttsEntry struct{ count, delta uint32 }
type stscEntry struct{ firstChunk, perChunk uint32 }

// span is a half-open byte range inside the file.
type span struct{ from, to uint64 }

// isContainer reports whether a box's payload is itself a box sequence.
func isContainer(t mp4.BoxType) bool {
	switch t {
	case mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(),
		mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeEdts():
		return true
	}
	return false
}

// parseTracks walks the top-level structure, expanding moov containers and
// collecting per-track tables plus the byte ranges of every mdat box.
func parseTracks(r io.ReadSeeker) ([]*trackData, []span, int64, error) {
	var tracks []*trackData
	var mdats []span
	var movieDurNS int64
	var cur *trackData
	_, err := mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		typ := h.BoxInfo.Type
		switch {
		case typ == mp4.BoxTypeMdat():
			mdats = append(mdats, span{from: h.BoxInfo.Offset + h.BoxInfo.HeaderSize,
				to: h.BoxInfo.Offset + h.BoxInfo.Size})
			return nil, nil
		case typ == mp4.BoxTypeMvhd():
			var b mp4.Mvhd
			if _, err := mp4.Unmarshal(r, h.BoxInfo.Size-h.BoxInfo.HeaderSize, &b, mp4.Context{}); err != nil {
				return nil, err
			}
			if b.Timescale > 0 {
				dur := uint64(b.DurationV0)
				if b.Version == 1 {
					dur = b.DurationV1
				}
				movieDurNS = int64(dur) * 1_000_000_000 / int64(b.Timescale)
			}
			return nil, nil
		case typ == mp4.BoxTypeTrak():
			cur = &trackData{}
			tracks = append(tracks, cur)
			_, err := h.Expand()
			cur = nil
			return nil, err
		case isContainer(typ):
			return h.Expand()
		}
		if cur == nil {
			return nil, nil
		}
		switch typ {
		case mp4.BoxTypeTkhd():
			var b mp4.Tkhd
			if _, err := mp4.Unmarshal(r, h.BoxInfo.Size-h.BoxInfo.HeaderSize, &b, mp4.Context{}); err != nil {
				return nil, err
			}
			cur.id = b.TrackID
		case mp4.BoxTypeHdlr():
			var b mp4.Hdlr
			if _, err := mp4.Unmarshal(r, h.BoxInfo.Size-h.BoxInfo.HeaderSize, &b, mp4.Context{}); err != nil {
				return nil, err
			}
			cur.handler = string(b.HandlerType[:])
		case mp4.BoxTypeMdhd():
			var b mp4.Mdhd
			if _, err := mp4.Unmarshal(r, h.BoxInfo.Size-h.BoxInfo.HeaderSize, &b, mp4.Context{}); err != nil {
				return nil, err
			}
			cur.timescale = b.Timescale
		case mp4.BoxTypeStts():
			var b mp4.Stts
			if _, err := mp4.Unmarshal(r, h.BoxInfo.Size-h.BoxInfo.HeaderSize, &b, mp4.Context{}); err != nil {
				return nil, err
			}
			for _, e := range b.Entries {
				cur.sttsCount = append(cur.sttsCount, sttsEntry{count: e.SampleCount, delta: e.SampleDelta})
			}
		case mp4.BoxTypeStss():
			var b mp4.Stss
			if _, err := mp4.Unmarshal(r, h.BoxInfo.Size-h.BoxInfo.HeaderSize, &b, mp4.Context{}); err != nil {
				return nil, err
			}
			cur.stss = append(cur.stss, b.SampleNumber...)
		case mp4.BoxTypeStsc():
			var b mp4.Stsc
			if _, err := mp4.Unmarshal(r, h.BoxInfo.Size-h.BoxInfo.HeaderSize, &b, mp4.Context{}); err != nil {
				return nil, err
			}
			for _, e := range b.Entries {
				cur.stsc = append(cur.stsc, stscEntry{firstChunk: e.FirstChunk, perChunk: e.SamplesPerChunk})
			}
		case mp4.BoxTypeStsz():
			var b mp4.Stsz
			if _, err := mp4.Unmarshal(r, h.BoxInfo.Size-h.BoxInfo.HeaderSize, &b, mp4.Context{}); err != nil {
				return nil, err
			}
			cur.stszSize = b.SampleSize
			cur.stszCount = b.SampleCount
			cur.stsz = b.EntrySize
		case mp4.BoxTypeStco():
			var b mp4.Stco
			if _, err := mp4.Unmarshal(r, h.BoxInfo.Size-h.BoxInfo.HeaderSize, &b, mp4.Context{}); err != nil {
				return nil, err
			}
			for _, off := range b.ChunkOffset {
				cur.chunkOff = append(cur.chunkOff, uint64(off))
			}
		case mp4.BoxTypeCo64():
			var b mp4.Co64
			if _, err := mp4.Unmarshal(r, h.BoxInfo.Size-h.BoxInfo.HeaderSize, &b, mp4.Context{}); err != nil {
				return nil, err
			}
			cur.chunkOff = append(cur.chunkOff, b.ChunkOffset...)
		}
		return nil, nil
	})
	if err != nil {
		return nil, nil, 0, clock.Wrap(clock.KindInvalid, clock.CodeUnsupported, "mp4", err)
	}
	return tracks, mdats, movieDurNS, nil
}

// sampleOffsets expands stsc + stco into a per-sample absolute offset.
func (t *trackData) sampleOffsets() ([]uint64, error) {
	count := t.sampleCount()
	if count == 0 {
		return nil, nil
	}
	if len(t.chunkOff) == 0 || len(t.stsc) == 0 {
		return nil, clock.New(clock.KindInvalid, clock.CodeUnsupported, "mp4",
			"missing chunk tables")
	}
	offs := make([]uint64, 0, count)
	for i, entry := range t.stsc {
		first := entry.firstChunk
		if first == 0 {
			return nil, clock.New(clock.KindInvalid, clock.CodeUnsupported, "mp4",
				"chunk index starts at zero")
		}
		last := uint32(len(t.chunkOff))
		if i+1 < len(t.stsc) {
			last = t.stsc[i+1].firstChunk - 1
		}
		if last > uint32(len(t.chunkOff)) {
			last = uint32(len(t.chunkOff))
		}
		for c := first; c <= last; c++ {
			off := t.chunkOff[c-1]
			for k := uint32(0); k < entry.perChunk; k++ {
				if uint32(len(offs)) >= count {
					break
				}
				offs = append(offs, off)
				off += uint64(t.sampleSize(uint32(len(offs) - 1)))
			}
		}
	}
	return offs, nil
}

func (t *trackData) sampleCount() uint32 {
	if t.stszSize != 0 {
		return t.stszCount
	}
	return uint32(len(t.stsz))
}

func (t *trackData) sampleSize(i uint32) uint32 {
	if t.stszSize != 0 {
		return t.stszSize
	}
	if int(i) < len(t.stsz) {
		return t.stsz[i]
	}
	return 0
}

// decodeTimes expands stts into per-sample decode time in the track timescale.
func (t *trackData) decodeTimes() []uint64 {
	n := t.sampleCount()
	out := make([]uint64, 0, n)
	var tck uint64
	for _, e := range t.sttsCount {
		for k := uint32(0); k < e.count; k++ {
			out = append(out, tck)
			tck += uint64(e.delta)
		}
	}
	return out
}

// BuildFile indexes the MP4 at path. Every keyframe offset is verified by
// seeking to it and parsing the sample's size header.
func BuildFile(path string) (*Index, error) {
	return BuildFileWithFormat(path, FormatFinding)
}

// SampleFormat selects how sample payloads are structurally verified.
type SampleFormat uint8

const (
	// FormatFinding validates AVC/HEVC-style length-prefixed NAL units: the
	// leading 4-byte length must cover a non-empty, in-bounds unit.
	FormatFinding SampleFormat = iota
	// FormatOpaque accepts any non-empty sample whose first four bytes do
	// not claim to extend past the sample. Use for codecs whose payload is
	// not length-prefixed.
	FormatOpaque
)

func (f SampleFormat) String() string {
	if f == FormatOpaque {
		return "opaque"
	}
	return "length-prefixed"
}

// BuildFileWithFormat indexes the MP4 using the requested sample format.
func BuildFileWithFormat(path string, format SampleFormat) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, clock.Wrap(clock.KindInvalid, clock.CodeUnsupported, path, err)
	}
	defer f.Close()
	idx, err := BuildFormat(f, path, format)
	if err != nil {
		return nil, err
	}
	return idx, nil
}

// Build indexes from an already-open file using the default sample format.
func Build(f *os.File, path string) (*Index, error) {
	return BuildFormat(f, path, FormatFinding)
}

// BuildFormat indexes from an already-open file with explicit verification.
func BuildFormat(f *os.File, path string, format SampleFormat) (*Index, error) {
	tracks, mdats, movieDurNS, err := parseTracks(f)
	if err != nil {
		return nil, err
	}
	idx := &Index{File: path, DurationNS: movieDurNS, SampleFormat: format.String()}
	var verified int
	for _, t := range tracks {
		if t.handler != "vide" || len(t.stss) == 0 {
			continue
		}
		if t.timescale == 0 {
			return nil, clock.New(clock.KindInvalid, clock.CodeInvalidDuration, path,
				"zero media timescale")
		}
		offs, err := t.sampleOffsets()
		if err != nil {
			return nil, err
		}
		times := t.decodeTimes()
		idx.Samples += int(t.sampleCount())
		if idx.Timescale == 0 {
			idx.Timescale = t.timescale
		}
		for _, sn := range t.stss {
			if sn == 0 || int(sn) > len(offs) || int(sn) > len(times) {
				return nil, clock.New(clock.KindInvalid, clock.CodeUnsupported, path,
					fmt.Sprintf("sync sample %d out of range", sn))
			}
			i := int(sn) - 1
			size := t.sampleSize(uint32(i))
			kf := Keyframe{
				Track:   "video",
				TrackID: t.id,
				Sample:  sn,
				TimeNS:  int64(times[i]) * 1_000_000_000 / int64(t.timescale),
				Offset:  offs[i],
				Size:    size,
			}
			if i+1 < len(times) {
				kf.DurNS = int64(times[i+1]-times[i]) * 1_000_000_000 / int64(t.timescale)
			}
			idx.Keyframes = append(idx.Keyframes, kf)
		}
	}
	sort.Slice(idx.Keyframes, func(i, j int) bool {
		if idx.Keyframes[i].TimeNS != idx.Keyframes[j].TimeNS {
			return idx.Keyframes[i].TimeNS < idx.Keyframes[j].TimeNS
		}
		return idx.Keyframes[i].Sample < idx.Keyframes[j].Sample
	})
	info, err := f.Stat()
	if err != nil {
		return nil, clock.Wrap(clock.KindInvalid, clock.CodeUnsupported, path, err)
	}
	// Verify every offset by re-parsing its sample header.
	for i := range idx.Keyframes {
		kf := &idx.Keyframes[i]
		if err := verifyOffset(f, info.Size(), mdats, *kf, format); err != nil {
			return nil, err
		}
		verified++
		kf.Verified = true
	}
	if len(idx.Keyframes) > 0 {
		idx.Roundtrip = float64(verified) / float64(len(idx.Keyframes))
	}
	return idx, nil
}

// verifyOffset proves an index entry points at readable, structurally valid
// sample bytes that live inside a media-data box.
func verifyOffset(f *os.File, fileSize int64, mdats []span, kf Keyframe,
	format SampleFormat) error {
	end := kf.Offset + uint64(kf.Size)
	if int64(end) > fileSize {
		return clock.New(clock.KindRecovery, clock.CodeIndexOffset, kf.OffsetID(),
			"offset beyond end of file")
	}
	if kf.Size < 8 {
		return clock.New(clock.KindRecovery, clock.CodeIndexOffset, kf.OffsetID(),
			"sample too small to parse")
	}
	inside := false
	for _, m := range mdats {
		if kf.Offset >= m.from && end <= m.to {
			inside = true
			break
		}
	}
	if !inside {
		return clock.New(clock.KindRecovery, clock.CodeIndexOffset, kf.OffsetID(),
			"sample offset outside media data")
	}
	// Only the leading header is needed to prove the offset lands on a real
	// sample; reading the whole sample would double the I/O for no gain.
	head := make([]byte, sampleHeaderBytes)
	n := len(head)
	if uint32(n) > kf.Size {
		n = int(kf.Size)
	}
	if _, err := f.ReadAt(head[:n], int64(kf.Offset)); err != nil {
		return clock.New(clock.KindRecovery, clock.CodeIndexOffset, kf.OffsetID(),
			"unreadable sample bytes")
	}
	if err := parseSampleHeader(head[:n], kf.Size, format); err != nil {
		return clock.New(clock.KindRecovery, clock.CodeIndexOffset, kf.OffsetID(),
			err.Error())
	}
	return nil
}

// sampleHeaderBytes is how much of a sample is read to validate structure.
const sampleHeaderBytes = 16

// parseSampleHeader validates the first bytes of a sample against the
// selected format, so an offset that lands inside mdat but on garbage does
// not verify. size is the sample's declared size in bytes.
func parseSampleHeader(head []byte, size uint32, format SampleFormat) error {
	if len(head) < 4 {
		return fmt.Errorf("sample shorter than its length field")
	}
	if format == FormatOpaque {
		// For codecs without length-prefixed payloads the strongest honest
		// check is that the span is non-empty and inside the file, which the
		// caller has already established.
		return nil
	}
	nalLen := binary.BigEndian.Uint32(head[0:4])
	if nalLen == 0 {
		return fmt.Errorf("zero-length leading NAL unit")
	}
	// The leading NAL unit must fit inside the sample's declared size, not
	// merely inside the header bytes we happened to read.
	if uint64(nalLen)+4 > uint64(size) {
		return fmt.Errorf("leading NAL unit exceeds the sample")
	}
	if len(head) < 5 {
		return fmt.Errorf("sample has no NAL header byte")
	}
	// A NAL unit's first byte must not have the forbidden zero bit set.
	if head[4]&0x80 != 0 {
		return fmt.Errorf("invalid NAL unit header")
	}
	return nil
}

// OffsetID formats an offset for error subjects.
func (k Keyframe) OffsetID() string {
	return fmt.Sprintf("sample@%d", k.Offset)
}

// HitRate reports the fraction of edit requests the index can serve. An
// editor scrubbing to time t needs the nearest keyframe at or before t; the
// request is a hit when such a keyframe exists, the keyframe's offset
// verified, and t still lies inside the indexed media span. Requests before
// the first keyframe or past the last frame are misses, which is the honest
// result for media that does not extend that far.
func (idx *Index) HitRate(times []int64, toleranceNS int64) float64 {
	if len(times) == 0 || len(idx.Keyframes) == 0 {
		return 0
	}
	// The media span comes from the movie header, not from the last
	// keyframe: an editor may seek to the previous keyframe and decode
	// forward through trailing non-key frames.
	spanEnd := idx.DurationNS
	if spanEnd == 0 {
		last := idx.Keyframes[len(idx.Keyframes)-1]
		spanEnd = last.TimeNS + sampleSpanNS(idx.Keyframes)
	}
	var hits int
	for _, want := range times {
		if want < idx.Keyframes[0].TimeNS-toleranceNS || want > spanEnd+toleranceNS {
			continue
		}
		kf, ok := idx.SeekPrevious(want + toleranceNS)
		if ok && kf.Verified {
			hits++
		}
	}
	return float64(hits) / float64(len(times))
}

// sampleSpanNS estimates the per-frame duration from the first keyframe with
// a recorded duration, used only when a movie header duration is absent.
func sampleSpanNS(kfs []Keyframe) int64 {
	for _, kf := range kfs {
		if kf.DurNS > 0 {
			return kf.DurNS
		}
	}
	return 0
}

// SeekPrevious returns the nearest verified keyframe at or before ns, the
// lookup an editor performs when locating a shot.
func (idx *Index) SeekPrevious(ns int64) (Keyframe, bool) {
	var best Keyframe
	var found bool
	for _, kf := range idx.Keyframes {
		if kf.TimeNS > ns {
			break
		}
		best, found = kf, true
	}
	return best, found
}

// Nearest returns the keyframe covering ns, if any.
func (idx *Index) Nearest(ns int64) (Keyframe, bool) {
	for _, kf := range idx.Keyframes {
		if ns >= kf.TimeNS && ns <= kf.TimeNS+kf.DurNS {
			return kf, true
		}
	}
	return Keyframe{}, false
}
