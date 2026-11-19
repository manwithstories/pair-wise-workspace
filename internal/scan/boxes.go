// Package scan streams MP4/MOV container structure and sample tables.
// Memory use is bounded by the sample-table size of the media, never by
// reading mdat payloads; oversized or out-of-range boxes are rejected
// before any allocation.
package scan

import (
	"errors"
	"fmt"
	"io"

	"aircheck/internal/fault"

	mp4 "github.com/abema/go-mp4"
)

// SttsEntry is one time-to-sample run.
type SttsEntry struct {
	Count uint32
	Delta uint32
}

// StscEntry is one sample-to-chunk run.
type StscEntry struct {
	FirstChunk      uint32
	SamplesPerChunk uint32
}

// TrackInfo holds the parsed tables of one track.
type TrackInfo struct {
	ID           uint32
	Timescale    uint32
	Duration     int64 // in track timescale units
	MediaType    string // "video", "audio", or "other"
	SampleSizes  []uint32
	ChunkOffsets []uint64
	Stsc         []StscEntry
	Stts         []SttsEntry
	SyncSamples  map[uint32]bool // nil means every sample is a keyframe
}

// ContainerInfo is the structural summary of one MP4/MOV file.
type ContainerInfo struct {
	MajorBrand     string
	MovieTimescale uint32
	Duration       int64 // movie timescale units
	Tracks         []*TrackInfo
	Faults         fault.List
	// DamageOffset is the raw byte offset of the first out-of-range or
	// truncated box; -1 when the structure is intact.
	DamageOffset int64
}

// maxBoxPayload caps any single box payload we will buffer (64 MiB).
const maxBoxPayload = 64 << 20

// Parse walks the box tree of r (size bytes) and returns the container
// summary. It never reads mdat payloads.
func Parse(r io.ReadSeeker, size int64) (*ContainerInfo, error) {
	ci := &ContainerInfo{DamageOffset: -1}
	var cur *TrackInfo

	_, err := mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		bi := h.BoxInfo
		// Reject oversized or out-of-range boxes before any allocation.
		if bi.Size > maxBoxPayload && bi.Type != mp4.BoxTypeMdat() {
			ci.recordDamage(fault.KindBoxOversized, int64(bi.Offset),
				fmt.Sprintf("box %s size %d exceeds limit", bi.Type, bi.Size))
			return nil, errStop
		}
		if int64(bi.Offset)+int64(bi.Size) > size {
			ci.recordDamage(fault.KindBoxOutOfRange, int64(bi.Offset),
				fmt.Sprintf("box %s [%d,%d) beyond file size %d", bi.Type, bi.Offset, bi.Offset+bi.Size, size))
			return nil, errStop
		}

		switch bi.Type {
		case mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(),
			mp4.BoxTypeMinf(), mp4.BoxTypeStbl():
			return h.Expand()
		case mp4.BoxTypeFtyp():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			if ftyp, ok := box.(*mp4.Ftyp); ok {
				ci.MajorBrand = string(ftyp.MajorBrand[:])
			}
		case mp4.BoxTypeMvhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			mvhd := box.(*mp4.Mvhd)
			ci.MovieTimescale = mvhd.Timescale
			if mvhd.GetVersion() == 1 {
				ci.Duration = int64(mvhd.DurationV1)
			} else {
				ci.Duration = int64(mvhd.DurationV0)
			}
		case mp4.BoxTypeTkhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			tkhd := box.(*mp4.Tkhd)
			cur = &TrackInfo{ID: tkhd.TrackID, MediaType: "other"}
			ci.Tracks = append(ci.Tracks, cur)
		case mp4.BoxTypeMdhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			mdhd := box.(*mp4.Mdhd)
			if cur != nil {
				cur.Timescale = mdhd.Timescale
				if mdhd.GetVersion() == 1 {
					cur.Duration = int64(mdhd.DurationV1)
				} else {
					cur.Duration = int64(mdhd.DurationV0)
				}
			}
		case mp4.BoxTypeHdlr():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			hdlr := box.(*mp4.Hdlr)
			if cur != nil {
				switch string(hdlr.HandlerType[:]) {
				case "vide":
					cur.MediaType = "video"
				case "soun":
					cur.MediaType = "audio"
				}
			}
		case mp4.BoxTypeStts():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			for _, e := range box.(*mp4.Stts).Entries {
				cur.Stts = append(cur.Stts, SttsEntry{Count: e.SampleCount, Delta: e.SampleDelta})
			}
		case mp4.BoxTypeStsz():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			stsz := box.(*mp4.Stsz)
			cur.SampleSizes = append(cur.SampleSizes, stsz.EntrySize...)
		case mp4.BoxTypeStsc():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			for _, e := range box.(*mp4.Stsc).Entries {
				cur.Stsc = append(cur.Stsc, StscEntry{FirstChunk: e.FirstChunk, SamplesPerChunk: e.SamplesPerChunk})
			}
		case mp4.BoxTypeStco():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			for _, o := range box.(*mp4.Stco).ChunkOffset {
				cur.ChunkOffsets = append(cur.ChunkOffsets, uint64(o))
			}
		case mp4.BoxTypeCo64():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			cur.ChunkOffsets = append(cur.ChunkOffsets, box.(*mp4.Co64).ChunkOffset...)
		case mp4.BoxTypeStss():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			cur.SyncSamples = map[uint32]bool{}
			for _, n := range box.(*mp4.Stss).SampleNumber {
				cur.SyncSamples[n] = true
			}
		}
		return nil, nil
	})
	if err != nil && !errors.Is(err, errStop) {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			off, _ := r.Seek(0, io.SeekCurrent)
			ci.recordDamage(fault.KindTruncated, off, "unexpected end of file while parsing boxes")
		} else {
			return ci, fmt.Errorf("parse boxes: %w", err)
		}
	}
	if len(ci.Tracks) == 0 && ci.DamageOffset < 0 {
		ci.recordDamage(fault.KindMissingIndex, 0, "no tracks found (moov missing or unreadable)")
	}
	return ci, nil
}

var errStop = errors.New("scan: stop walk at damaged box")

func (ci *ContainerInfo) recordDamage(kind fault.Kind, offset int64, msg string) {
	if ci.DamageOffset < 0 {
		ci.DamageOffset = offset
	}
	ci.Faults = append(ci.Faults, fault.New(kind, fault.StageScan, 0, 0, offset,
		kind == fault.KindMissingIndex || kind == fault.KindTruncated, msg))
}
