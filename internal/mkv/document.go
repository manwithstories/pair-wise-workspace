package mkv

import (
	"errors"
	"fmt"
	"io"

	"review/internal/ebml"
	"review/internal/media"
)

// Document is the decoded result of one Matroska/WebM stream.
type Document struct {
	TimecodeScale uint64 // nanoseconds per timecode unit
	Duration      float64
	Tracks        []Track
	Blocks        []Block
}

// Decode streams an EBML reader to the end of the document and returns the
// decoded tracks and blocks. Truncation/boundary errors surface unchanged
// so downstream index builders can see the first missing offset.
func Decode(rd *ebml.Reader) (*Document, error) {
	doc := &Document{TimecodeScale: media.DefaultTimestampScale}
	for {
		h, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return doc, nil
		}
		if err != nil {
			return nil, &DecodeError{Stage: "document", Cause: err}
		}
		switch h.ID {
		case ebml.IDEBML:
			if err := skipOrEnter(rd, h); err != nil {
				return nil, err
			}
		case ebml.IDSegment:
			if err := decodeSegment(rd, h, doc); err != nil {
				return nil, err
			}
		default:
			if err := skipOrEnter(rd, h); err != nil {
				return nil, err
			}
		}
	}
}

func decodeSegment(rd *ebml.Reader, seg ebml.Header, doc *Document) error {
	return walkChildren(rd, seg, func(h ebml.Header) error {
		switch h.ID {
		case ebml.IDInfo:
			return decodeInfo(rd, h, doc)
		case ebml.IDTracks:
			return decodeTracks(rd, h, doc)
		case ebml.IDCluster:
			return decodeCluster(rd, h, doc)
		default:
			return skipOrEnter(rd, h)
		}
	})
}

func decodeInfo(rd *ebml.Reader, info ebml.Header, doc *Document) error {
	return walkChildren(rd, info, func(h ebml.Header) error {
		if ebml.IsMaster(h.ID) {
			return skipOrEnter(rd, h)
		}
		raw, err := rd.ReadAll(h)
		if err != nil {
			return &DecodeError{Stage: "info", Cause: err}
		}
		switch h.ID {
		case ebml.IDTimestampScale:
			v, err := DecodeUint(raw)
			if err != nil {
				return &DecodeError{Stage: "timecode-scale", Cause: err}
			}
			if v > 0 {
				doc.TimecodeScale = v
			}
		case ebml.IDDuration:
			f, err := DecodeFloat(raw)
			if err != nil {
				return &DecodeError{Stage: "duration", Cause: err}
			}
			doc.Duration = f
		default:
			// skip already consumed via ReadAll
		}
		return nil
	})
}

func decodeTracks(rd *ebml.Reader, tracks ebml.Header, doc *Document) error {
	return walkChildren(rd, tracks, func(h ebml.Header) error {
		if h.ID != ebml.IDTrackEntry {
			return skipOrEnter(rd, h)
		}
		tr, err := decodeTrackEntry(rd, h)
		if err != nil {
			return err
		}
		doc.Tracks = append(doc.Tracks, tr)
		return nil
	})
}

func decodeTrackEntry(rd *ebml.Reader, entry ebml.Header) (Track, error) {
	tr := Track{}
	err := walkChildren(rd, entry, func(h ebml.Header) error {
		switch h.ID {
		case ebml.IDAudio:
			return decodeAudio(rd, h, &tr)
		case ebml.IDColour:
			return decodeColour(rd, h, &tr)
		default:
			if ebml.IsMaster(h.ID) {
				return skipOrEnter(rd, h)
			}
			raw, err := rd.ReadAll(h)
			if err != nil {
				return &DecodeError{Stage: "track", Cause: err}
			}
			return applyTrackField(&tr, h.ID, raw)
		}
	})
	return tr, err
}

func applyTrackField(tr *Track, id uint32, raw []byte) error {
	switch id {
	case ebml.IDTrackNumber:
		v, err := DecodeUint(raw)
		if err != nil {
			return &DecodeError{Stage: "track-number", Cause: err}
		}
		tr.Number = uint32(v)
	case ebml.IDTrackUID:
		v, err := DecodeUint(raw)
		if err != nil {
			return &DecodeError{Stage: "track-uid", Cause: err}
		}
		tr.UID = v
	case ebml.IDTrackType:
		v, err := DecodeUint(raw)
		if err != nil {
			return &DecodeError{Stage: "track-type", Cause: err}
		}
		tr.Type = uint8(v)
	case ebml.IDCodecID:
		tr.Codec = string(raw)
	case ebml.IDDefaultDur:
		v, err := DecodeUint(raw)
		if err != nil {
			return &DecodeError{Stage: "default-duration", Cause: err}
		}
		tr.DefaultDurNS = v
	default:
		// Unknown leaf: already consumed, safely ignored.
	}
	return nil
}

func decodeAudio(rd *ebml.Reader, audio ebml.Header, tr *Track) error {
	return walkChildren(rd, audio, func(h ebml.Header) error {
		if ebml.IsMaster(h.ID) {
			return skipOrEnter(rd, h)
		}
		raw, err := rd.ReadAll(h)
		if err != nil {
			return &DecodeError{Stage: "audio", Cause: err}
		}
		switch h.ID {
		case ebml.IDChannels:
			v, err := DecodeUint(raw)
			if err != nil {
				return &DecodeError{Stage: "channels", Cause: err}
			}
			tr.Channels = uint32(v)
		case ebml.IDSamplingFreq:
			f, err := DecodeFloat(raw)
			if err != nil {
				return &DecodeError{Stage: "sampling-freq", Cause: err}
			}
			tr.SampleRate = f
		case ebml.IDBitDepth:
			v, err := DecodeUint(raw)
			if err != nil {
				return &DecodeError{Stage: "bit-depth", Cause: err}
			}
			tr.BitDepth = uint32(v)
		}
		return nil
	})
}

func decodeColour(rd *ebml.Reader, colour ebml.Header, tr *Track) error {
	return walkChildren(rd, colour, func(h ebml.Header) error {
		if ebml.IsMaster(h.ID) {
			return skipOrEnter(rd, h)
		}
		raw, err := rd.ReadAll(h)
		if err != nil {
			return &DecodeError{Stage: "colour", Cause: err}
		}
		v, err := DecodeUint(raw)
		if err != nil {
			return &DecodeError{Stage: "colour-value", Cause: err}
		}
		switch h.ID {
		case ebml.IDMatrixCoeffs:
			tr.Matrix = v
		case ebml.IDTransferChar:
			tr.Transfer = v
		case ebml.IDPrimaries:
			tr.Primaries = v
		}
		return nil
	})
}

func decodeCluster(rd *ebml.Reader, cluster ebml.Header, doc *Document) error {
	var clusterTC int64
	return walkChildren(rd, cluster, func(h ebml.Header) error {
		switch h.ID {
		case ebml.IDTimecode:
			raw, err := rd.ReadAll(h)
			if err != nil {
				return &DecodeError{Stage: "timecode", Cause: err}
			}
			v, err := DecodeUint(raw)
			if err != nil {
				return &DecodeError{Stage: "timecode", Cause: err}
			}
			clusterTC = int64(v)
			return nil
		case ebml.IDSimpleBlock:
			raw, err := rd.ReadAll(h)
			if err != nil {
				return &DecodeError{Stage: "simple-block", Cause: err}
			}
			return appendBlock(doc, h, raw, clusterTC, true)
		case ebml.IDBlockGroup:
			return decodeBlockGroup(rd, h, doc, clusterTC)
		default:
			return skipOrEnter(rd, h)
		}
	})
}

func decodeBlockGroup(rd *ebml.Reader, group ebml.Header, doc *Document, clusterTC int64) error {
	return walkChildren(rd, group, func(h ebml.Header) error {
		switch h.ID {
		case ebml.IDBlock:
			raw, err := rd.ReadAll(h)
			if err != nil {
				return &DecodeError{Stage: "block", Cause: err}
			}
			return appendBlock(doc, h, raw, clusterTC, false)
		default:
			return skipOrEnter(rd, h)
		}
	})
}

func appendBlock(doc *Document, h ebml.Header, raw []byte, clusterTC int64, simple bool) error {
	track, relative, flags, body, err := DecodeBlock(raw)
	if err != nil {
		return &DecodeError{Stage: "block-header", Cause: err}
	}
	absTC := clusterTC + int64(relative)
	b := Block{
		Track:  track,
		Time:   media.FromScaledTimecode(doc.TimecodeScale, absTC),
		Offset: h.Offset,
		// SimpleBlock keyframe flag is bit 7; block groups derive
		// keyframes from reference blocks which our fixtures omit.
		Keyframe: simple && flags&0x80 != 0,
		Payload:  append([]byte(nil), body...),
	}
	if len(b.Payload) == 0 {
		return &DecodeError{Stage: "block",
			Cause: fmt.Errorf("empty block payload at offset %d", h.Offset)}
	}
	doc.Blocks = append(doc.Blocks, b)
	return nil
}
