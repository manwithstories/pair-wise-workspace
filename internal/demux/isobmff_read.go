package demux

import (
	"encoding/binary"
	"io"

	"github.com/abema/go-mp4"

	"relay/internal/media"
)

// MP4TrackInfo describes one track discovered in the init segment.
type MP4TrackInfo struct {
	SourceID  uint32
	Kind      media.Kind
	Timescale uint32
	Width     uint32
	Height    uint32
}

// rmtaPayload is the private "relay metadata, per fragment" box carried inside
// each traf. Standard players ignore unknown boxes; the relay reader uses it to
// recover capture anchors (clock ticks and reference nanoseconds).
type rmtaPayload struct {
	Sequence     uint64
	ClockTicks   uint64
	CaptureRefNS int64
	TimescaleHz  uint32
}

func encodeRMTA(p rmtaPayload) []byte {
	b := make([]byte, 32)
	bin := binary.BigEndian
	bin.PutUint64(b[0:8], p.Sequence)
	bin.PutUint64(b[8:16], p.ClockTicks)
	bin.PutUint64(b[16:24], uint64(p.CaptureRefNS))
	bin.PutUint32(b[24:28], p.TimescaleHz)
	return b
}

func decodeRMTA(b []byte) (rmtaPayload, error) {
	if len(b) < 28 {
		return rmtaPayload{}, media.NewError(media.ErrMetadata, 0, "short rmta box")
	}
	bin := binary.BigEndian
	return rmtaPayload{
		Sequence:     bin.Uint64(b[0:8]),
		ClockTicks:   bin.Uint64(b[8:16]),
		CaptureRefNS: int64(bin.Uint64(b[16:24])),
		TimescaleHz:  bin.Uint32(b[24:28]),
	}, nil
}

// ReadFragmentedMP4 parses an fMP4 stream, validating box framing, trun/mdat
// lengths, track consistency and per-sample hashes. Frames preserve their
// encoded bytes exactly; the returned map describes every discovered track.
func ReadFragmentedMP4(r io.ReadSeeker) ([]*media.RawFrame, map[uint32]MP4TrackInfo, error) {
	tracks := map[uint32]MP4TrackInfo{}
	var frames []*media.RawFrame
	var pending *fragMeta

	for {
		bi, err := mp4.ReadBoxInfo(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, media.NewError(media.ErrTruncated, 0, "top-level box: "+err.Error())
		}
		switch bi.Type {
		case mp4.BoxTypeMoov():
			infos, err := readMoovTracks(r, bi)
			if err != nil {
				return nil, nil, err
			}
			for _, ti := range infos {
				tracks[ti.SourceID] = ti
			}
		case mp4.BoxTypeMoof():
			pending, err = readMoof(r, bi)
			if err != nil {
				return nil, nil, err
			}
		case mp4.BoxTypeMdat():
			if pending == nil {
				return nil, nil, media.NewError(media.ErrMetadata, 0, "mdat without preceding moof")
			}
			data, err := readBoxPayload(r, bi)
			if err != nil {
				return nil, nil, err
			}
			fr, err := materialiseFragment(pending, data, tracks)
			if err != nil {
				return nil, nil, err
			}
			frames = append(frames, fr)
			pending = nil
		}
		if err := skipBox(r, bi); err != nil {
			return nil, nil, err
		}
	}
	if uint64(len(frames)) > media.MaxTotalSamples {
		return nil, nil, media.NewError(media.ErrLimitExceeded, 0, "sample count over limit")
	}
	return frames, tracks, nil
}

// fragMeta is everything the moof tells us about the single-sample fragment.
type fragMeta struct {
	TrackID    uint32
	DecodeTime uint64
	SampleSize uint32
	RMTA       *rmtaPayload
}

func readMoof(r io.ReadSeeker, moof *mp4.BoxInfo) (*fragMeta, error) {
	if _, err := moof.SeekToPayload(r); err != nil {
		return nil, err
	}
	meta := &fragMeta{}
	// Expect one traf child; walk all children of the moof.
	for {
		bi, err := mp4.ReadBoxInfo(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, media.NewError(media.ErrTruncated, 0, "moof child: "+err.Error())
		}
		if bi.Type == mp4.BoxTypeTraf() {
			if err := readTraf(r, bi, meta); err != nil {
				return nil, err
			}
		}
		if err := skipBox(r, bi); err != nil {
			return nil, err
		}
		if atEnd(r, moof) {
			break
		}
	}
	if meta.SampleSize == 0 || meta.SampleSize > media.MaxFrameBytes {
		return nil, media.NewError(media.ErrLengthMismatch, meta.TrackID, "trun sample size out of range")
	}
	return meta, nil
}

func readTraf(r io.ReadSeeker, traf *mp4.BoxInfo, meta *fragMeta) error {
	if _, err := traf.SeekToPayload(r); err != nil {
		return err
	}
	for {
		bi, err := mp4.ReadBoxInfo(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return media.NewError(media.ErrTruncated, 0, "traf child: "+err.Error())
		}
		payload, err := readBoxPayload(r, bi)
		if err != nil {
			return err
		}
		switch bi.Type {
		case mp4.BoxTypeTfhd():
			box, _, err := mp4.UnmarshalAny(bytesReader(payload), bi.Type, uint64(len(payload)), mp4.Context{})
			if err != nil {
				return media.NewError(media.ErrMetadata, 0, "tfhd: "+err.Error())
			}
			meta.TrackID = box.(*mp4.Tfhd).TrackID
		case mp4.BoxTypeTfdt():
			box, _, err := mp4.UnmarshalAny(bytesReader(payload), bi.Type, uint64(len(payload)), mp4.Context{})
			if err != nil {
				return media.NewError(media.ErrMetadata, 0, "tfdt: "+err.Error())
			}
			meta.DecodeTime = box.(*mp4.Tfdt).GetBaseMediaDecodeTime()
		case mp4.BoxTypeTrun():
			box, _, err := mp4.UnmarshalAny(bytesReader(payload), bi.Type, uint64(len(payload)), mp4.Context{})
			if err != nil {
				return media.NewError(media.ErrMetadata, 0, "trun: "+err.Error())
			}
			trun := box.(*mp4.Trun)
			if trun.SampleCount != 1 || len(trun.Entries) != 1 {
				return media.NewError(media.ErrMetadata, meta.TrackID, "only single-sample fragments supported")
			}
			meta.SampleSize = trun.Entries[0].SampleSize
		default:
			if bi.Type == rmtaBoxType() {
				dec, err := decodeRMTA(payload)
				if err != nil {
					return err
				}
				meta.RMTA = &dec
			}
		}
		if err := skipBox(r, bi); err != nil {
			return err
		}
		if atEnd(r, traf) {
			break
		}
	}
	return nil
}

func materialiseFragment(meta *fragMeta, mdat []byte, tracks map[uint32]MP4TrackInfo) (*media.RawFrame, error) {
	if uint32(len(mdat)) != meta.SampleSize {
		return nil, media.NewError(media.ErrLengthMismatch, meta.TrackID,
			"mdat length does not match trun sample size")
	}
	ti, ok := tracks[meta.TrackID]
	if !ok {
		return nil, media.NewError(media.ErrMapping, meta.TrackID, "fragment for unknown track")
	}
	payload := append([]byte(nil), mdat...)
	sum := hashBytes(payload)
	fm := media.FrameMeta{
		SourceID:    meta.TrackID,
		Sequence:    meta.DecodeTime,
		Size:        meta.SampleSize,
		SHA256:      sum,
		Width:       ti.Width,
		Height:      ti.Height,
		TimescaleHz: uint64(ti.Timescale),
	}
	if meta.RMTA != nil {
		fm.Sequence = meta.RMTA.Sequence
		fm.ClockTicks = meta.RMTA.ClockTicks
		fm.CaptureRefNS = meta.RMTA.CaptureRefNS
		if meta.RMTA.TimescaleHz != 0 {
			fm.TimescaleHz = uint64(meta.RMTA.TimescaleHz)
		}
	}
	return &media.RawFrame{Kind: ti.Kind, Payload: payload, Meta: fm}, nil
}

func readBoxPayload(r io.ReadSeeker, bi *mp4.BoxInfo) ([]byte, error) {
	if _, err := bi.SeekToPayload(r); err != nil {
		return nil, err
	}
	size := int64(bi.Size - bi.HeaderSize)
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, media.NewError(media.ErrTruncated, 0, bi.Type.String()+" payload: "+err.Error())
	}
	return buf, nil
}

func skipBox(r io.ReadSeeker, bi *mp4.BoxInfo) error {
	_, err := r.Seek(int64(bi.Offset+bi.Size), io.SeekStart)
	return err
}

func atEnd(r io.ReadSeeker, parent *mp4.BoxInfo) bool {
	pos, err := r.Seek(0, io.SeekCurrent)
	return err == nil && uint64(pos) >= parent.Offset+parent.Size
}

func bytesReader(b []byte) io.ReadSeeker { return bytesNewReader(b) }
