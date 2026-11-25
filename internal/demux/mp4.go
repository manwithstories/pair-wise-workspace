// Package demux reads MP4/MOV archive containers without decoding any media.
// It supports plain MP4, fragmented MP4 and QuickTime MOV, streams samples
// track by track and turns every structural problem into a model.Error with
// stage, track, offset and recoverability attached.
package demux

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"mediaarchive/internal/model"
)

// Track kinds used by the explicit mapping rules.
const (
	KindVideo = "video"
	KindAudio = "audio"
	KindHint  = "hint"
)

// Sample flags.
const (
	// FlagRTP marks a hint-track sample whose payload is one RTP packet.
	FlagRTP uint32 = 1 << iota
	// FlagKeyframe marks a sync sample.
	FlagKeyframe
	// FlagTruncated marks a sample whose payload runs past the mdat box.
	FlagTruncated
)

// TrackMeta describes one track as declared by the container.
type TrackMeta struct {
	ID        uint32
	Kind      string
	Timescale uint32
	// StartTime is the non-zero track start offset in track time units.
	StartTime uint64
	// DriftPPM and WrapUnits are measured clock anomalies carried from the
	// fixture/archive metadata; the clock package decides what is fixable.
	DriftPPM  int32
	WrapUnits uint32
	RTPCount  uint32 // number of RTP packets on a hint track
	CodecHint string
}

// Sample is one demuxed media packet. Payload is owned by the caller after
// Next returns; the demuxer never reuses the backing array.
type Sample struct {
	TrackID  uint32
	Seq      uint32 // 1-based original sequence within the track
	ChunkSeq uint32 // 1-based fragment/chunk sequence inside the file
	DTS      uint64 // decode time in track time units
	Duration uint32 // duration in track time units (VFR friendly)
	Offset   int64  // absolute byte offset of the payload in the input file
	Size     uint32 // declared payload size
	Flags    uint32
	Payload  []byte
	SHA256   [32]byte // filled by the pipeline after validation
	RTPSeq   uint32   // RTP sequence number for hint samples (else 0)
}

// Chunk is one fragmented (armf) or single (mdat) media data region.
type Chunk struct {
	Seq       uint32
	DataBegin int64 // absolute payload begin (after box header)
	DataEnd   int64 // absolute payload end (exclusive)
	HeaderOff int64 // absolute offset of the box header
}

// Container is the parsed, streamable view of an input file.
type Container struct {
	Path        string
	IsQuickTime bool
	Brands      []string
	Tracks      []TrackMeta
	Chunks      []Chunk
	// Samples holds the per-chunk sample table in track/seq order.
	Samples [][]Sample
	Size    int64
	// TruncatedAt >= 0 marks the header offset of a truncated final mdat.
	TruncatedAt int64
	// Truncation holds the recoverable boundary errors observed while
	// scanning; --on-damage=fail turns them into hard failures upstream.
	Truncation []model.Error
	// Damages lists every recovered event, in encounter order.
	Damages []Damage
	r       io.ReadSeeker
}

// rawHeader is the parsed 8/16 byte MP4 box header.
type rawHeader struct {
	Type       mp4.BoxType
	Offset     int64
	HeaderSize uint64
	Size       uint64 // payload-inclusive box size
	ExtendEOF  bool
}

// readHeader parses one box header and enforces all boundary rules.
// Sizes beyond the file, declared sizes under the header and 32-bit size==1
// boxes missing a largesize are rejected here, before any payload read.
func readHeader(r io.ReadSeeker, fileSize int64) (*rawHeader, error) {
	off, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, model.NewError(model.StageDemux, "cannot seek box header").Wrap(err)
	}
	var head [8]byte
	_, err = io.ReadFull(r, head[:])
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return nil, io.EOF
	}
	if err != nil {
		return nil, model.NewError(model.StageDemux, "cannot read box header").WithOffset(off).Wrap(err)
	}
	size32 := binary.BigEndian.Uint32(head[0:4])
	t := mp4.BoxType{head[4], head[5], head[6], head[7]}
	h := &rawHeader{Type: t, Offset: off, HeaderSize: 8}
	switch size32 {
	case 0:
		// Box extends to end of file.
		h.Size = uint64(fileSize - off)
		h.ExtendEOF = true
	case 1:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return nil, model.NewError(model.StageDemux, "truncated 64-bit box header").
				WithOffset(off).Recover().Wrap(err)
		}
		h.HeaderSize = 16
		h.Size = binary.BigEndian.Uint64(ext[:])
	default:
		h.Size = uint64(size32)
	}
	if h.Size < h.HeaderSize {
		return nil, model.NewError(model.StageDemux,
			fmt.Sprintf("box %s size %d smaller than header", t.String(), h.Size)).WithOffset(off)
	}
	end := int64(h.Offset) + int64(h.Size)
	if end < off || end > fileSize {
		// A box header that promises bytes beyond EOF is exactly the
		// signature of a truncated mdat: recoverable when the caller has a
		// damage policy, fatal for structural boxes like moov.
		return h, model.NewError(model.StageDemux,
			fmt.Sprintf("box %s out of bounds: offset=%d size=%d file=%d",
				t.String(), off, h.Size, fileSize)).WithOffset(off).Recover()
	}
	return h, nil
}

// scanTopLevel walks every top-level box, validating boundaries and invoking
// cb for each. A recoverable boundary error on the final box is forwarded to
// onTruncated instead of aborting the walk; every other error is fatal.
func scanTopLevel(r io.ReadSeeker, fileSize int64, cb func(*rawHeader) error,
	onTruncated func(*rawHeader, *model.Error) error) error {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return model.NewError(model.StageDemux, "cannot seek start").Wrap(err)
	}
	for {
		h, err := readHeader(r, fileSize)
		if err == io.EOF {
			return nil
		}
		if me, ok := model.AsModelError(err); ok && me.Recoverable && h != nil {
			if onTruncated == nil {
				return err
			}
			return onTruncated(h, me)
		}
		if err != nil {
			return err
		}
		if err := cb(h); err != nil {
			return err
		}
		next := int64(h.Offset) + int64(h.Size)
		if _, err := r.Seek(next, io.SeekStart); err != nil {
			return model.NewError(model.StageDemux, "cannot seek next box").
				WithOffset(next).Wrap(err)
		}
	}
}

func u16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }
func u32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
func u64(b []byte) uint64 { return binary.BigEndian.Uint64(b) }

func putU16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }
func putU32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }
func putU64(b []byte, v uint64) { binary.BigEndian.PutUint64(b, v) }
