// Package index builds a keyframe index from an MP4 sample file and verifies
// every entry by re-parsing the bytes at each recorded offset.
package index

import (
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
	File      string     `json:"file"`
	Hash      string     `json:"hash"`
	Timescale uint32     `json:"timescale"`
	Keyframes []Keyframe `json:"keyframes"`
	Samples   int        `json:"samples"`
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
func parseTracks(r io.ReadSeeker) ([]*trackData, []span, error) {
	var tracks []*trackData
	var mdats []span
	var cur *trackData
	_, err := mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		typ := h.BoxInfo.Type
		switch {
		case typ == mp4.BoxTypeMdat():
			mdats = append(mdats, span{from: h.BoxInfo.Offset + h.BoxInfo.HeaderSize,
				to: h.BoxInfo.Offset + h.BoxInfo.Size})
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
		return nil, nil, clock.Wrap(clock.KindInvalid, clock.CodeUnsupported, "mp4", err)
	}
	return tracks, mdats, nil
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
	f, err := os.Open(path)
	if err != nil {
		return nil, clock.Wrap(clock.KindInvalid, clock.CodeUnsupported, path, err)
	}
	defer f.Close()
	idx, err := Build(f, path)
	if err != nil {
		return nil, err
	}
	return idx, nil
}

// Build indexes from an already-open file. The hash is computed over the file
// bytes by the caller-supplied hasher when non-nil.
func Build(f *os.File, path string) (*Index, error) {
	tracks, mdats, err := parseTracks(f)
	if err != nil {
		return nil, err
	}
	idx := &Index{File: path}
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
		if err := verifyOffset(f, info.Size(), mdats, *kf); err != nil {
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

// verifyOffset proves an index entry points at readable sample bytes that
// actually live inside a media-data box.
func verifyOffset(f *os.File, fileSize int64, mdats []span, kf Keyframe) error {
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
	buf := make([]byte, 8)
	if _, err := f.ReadAt(buf, int64(kf.Offset)); err != nil {
		return clock.New(clock.KindRecovery, clock.CodeIndexOffset, kf.OffsetID(),
			"unreadable sample bytes")
	}
	return nil
}

// OffsetID formats an offset for error subjects.
func (k Keyframe) OffsetID() string {
	return fmt.Sprintf("sample@%d", k.Offset)
}

// HitRate reports the fraction of requested times that resolved to a
// keyframe within tolerance.
func (idx *Index) HitRate(times []int64, toleranceNS int64) float64 {
	if len(times) == 0 {
		return 0
	}
	var hits int
	for _, want := range times {
		for _, kf := range idx.Keyframes {
			margin := toleranceNS
			if kf.DurNS > margin {
				margin = kf.DurNS
			}
			if want >= kf.TimeNS-margin && want <= kf.TimeNS+margin {
				hits++
				break
			}
		}
	}
	return float64(hits) / float64(len(times))
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
