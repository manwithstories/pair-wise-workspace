// Package ingest streams ISO BMFF / fMP4 boxes off disk, parses the fragment
// boxes (mfhd/tfhd/tfdt/trun/mdat) and validates declared lengths and sample
// ranges. Every parse fault is reported as a media.Fault carrying the first
// erroneous byte offset so the offending fragment can be isolated.
package ingest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"relay/internal/media"
)

// Sample describes one sample entry reconstructed from a trun box.
type Sample struct {
	Index               uint32
	Duration            uint32
	Size                uint32
	Flags               uint32
	CompositionOffset   int32 // signed (trun version 1 semantics)
	DataOffset          int64 // absolute byte offset of the sample inside mdat
	Sync                bool  // sample is a random-access point (flags bit 16 clear)
}

// Fragment is the parsed view of one moof+mdat fragment. Mdat aliases the
// backing Raw slice (no extra copy); callers must treat both as read-only.
type Fragment struct {
	TrackID     uint32
	Sequence    uint32 // mfhd.sequence_number
	Tfdt        uint64 // baseMediaDecodeTime in track timescale
	MoofOffset  int64  // absolute offset of the moof header in the source
	RawSize     int64  // total moof+mdat byte length
	Samples     []Sample
	Mdat        []byte
	Raw         []byte
	Source      string
}

// BoxHeader is the decoded 8/16-byte ISO BMFF box header.
type BoxHeader struct {
	Type   mp4.BoxType
	Size   uint64 // payload-inclusive total size
	Header uint64 // 8 or 16 bytes
	Offset int64  // header offset relative to the start of the stream
}

const (
	smallHeader = 8
	largeHeader = 16
)

// ReadBoxHeader decodes one box header from r, returning io.EOF only when no
// byte was available at all. A header whose declared length cannot fit its
// own header or the following payload yields a media.Fault with the offset.
func ReadBoxHeader(r io.Reader, base int64) (BoxHeader, error) {
	var word [8]byte
	n, err := io.ReadFull(r, word[:])
	if err != nil {
		return BoxHeader{}, err
	}
	h := BoxHeader{Offset: base, Header: smallHeader}
	copy(h.Type[:], word[4:])
	size32 := binary.BigEndian.Uint32(word[:4])
	switch size32 {
	case 1: // 64-bit largesize
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return h, media.NewFault(media.FaultTruncated, 0, 0, base+int64(n),
				"large-size box header truncated")
		}
		h.Header = largeHeader
		h.Size = binary.BigEndian.Uint64(ext[:])
	case 0: // box extends to end of stream; caller decides validity
		h.Size = 0
	default:
		h.Size = uint64(size32)
	}
	if h.Size != 0 && h.Size < h.Header {
		return h, media.NewFault(media.FaultBadLength, 0, 0, base,
			fmt.Sprintf("box %s size %d smaller than header", h.Type, h.Size))
	}
	return h, nil
}

// ParseFragment parses one complete moof+mdat byte sequence. The first
// error offset is always populated on failure.
func ParseFragment(source string, moofOffset int64, raw []byte) (*Fragment, error) {
	f := &Fragment{Source: source, MoofOffset: moofOffset, Raw: raw}
	r := bytes.NewReader(raw)

	moofH, err := ReadBoxHeader(r, 0)
	if err != nil {
		return nil, media.NewFault(media.FaultTruncated, 0, 0, moofOffset, "moof header unreadable")
	}
	if moofH.Type != mp4.BoxTypeMoof() {
		return nil, media.NewFault(media.FaultStructure, 0, 0, moofOffset,
			"fragment must start with moof, got "+moofH.Type.String())
	}
	if moofH.Size > uint64(len(raw)) {
		return nil, media.NewFault(media.FaultTruncated, 0, 0, moofOffset,
			fmt.Sprintf("moof declares %d bytes but fragment has %d", moofH.Size, len(raw)))
	}

	mfhd, tfhd, tfdt, trun, perr := parseMoof(r, moofH, moofOffset)
	if perr != nil {
		return nil, perr
	}
	f.Sequence = mfhd.SequenceNumber
	f.TrackID = tfhd.TrackID
	f.Tfdt = tfdt.GetBaseMediaDecodeTime()

	// mdat starts immediately after the moof box. All header reads below use
	// raw-relative offsets; the source offset is added only in faults.
	mdatRel := int64(moofH.Size)
	if mdatRel >= int64(len(raw)) {
		return nil, media.NewFault(media.FaultTruncated, f.TrackID, f.Sequence,
			moofOffset+mdatRel, "mdat box missing after moof")
	}
	if _, err := r.Seek(mdatRel, io.SeekStart); err != nil {
		return nil, err
	}
	mdatH, err := ReadBoxHeader(r, mdatRel)
	if err != nil {
		return nil, media.NewFault(media.FaultTruncated, f.TrackID, f.Sequence,
			moofOffset+mdatRel, "mdat header truncated")
	}
	if mdatH.Type != mp4.BoxTypeMdat() {
		return nil, media.NewFault(media.FaultStructure, f.TrackID, f.Sequence,
			moofOffset+mdatRel, "expected mdat, got "+mdatH.Type.String())
	}
	mdatPayloadOff := mdatRel + int64(mdatH.Header)
	if mdatH.Size == 0 {
		mdatH.Size = uint64(len(raw) - int(mdatRel))
	}
	if mdatH.Size > uint64(len(raw)-int(mdatRel)) {
		return nil, media.NewFault(media.FaultTruncated, f.TrackID, f.Sequence,
			moofOffset+mdatRel, fmt.Sprintf("mdat declares %d bytes but only %d remain",
				mdatH.Size, len(raw)-int(mdatRel)))
	}
	payloadLen := int64(mdatH.Size) - int64(mdatH.Header)
	f.Mdat = raw[mdatPayloadOff : mdatPayloadOff+payloadLen]
	f.RawSize = int64(mdatH.Size) + int64(moofH.Size)

	if err := buildSamples(f, trun, moofH, mdatH, moofOffset, payloadLen); err != nil {
		return nil, err
	}
	return f, nil
}
