package demux

import (
	"crypto/sha256"
	"errors"
	"io"

	"github.com/abema/go-mp4"

	"relay/internal/media"
)

// WriteFragmentedMP4 serialises frames of one source into a fragmented MP4
// stream. Every encoded payload is copied byte-for-byte; trun sample sizes and
// the mdat payload are derived from the same bytes the reader will re-hash.
func WriteFragmentedMP4(w io.WriteSeeker, hdr FSEQHeader, frames []*media.RawFrame) error {
	if uint32(len(frames)) > media.MaxTotalSamples {
		return media.NewError(media.ErrLimitExceeded, hdr.SourceID, "frame count over limit")
	}
	mw := mp4.NewWriter(w)
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStyp()}); err != nil {
		return err
	}
	styp := &mp4.Styp{
		MajorBrand:       brand("iso5"),
		MinorVersion:     1,
		CompatibleBrands: []mp4.CompatibleBrandElem{{CompatibleBrand: brand("iso5")}, {CompatibleBrand: brand("dash")}},
	}
	if _, err := mp4.Marshal(mw, styp, mp4.Context{}); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil {
		return err
	}
	if err := writeMoov(mw, hdr); err != nil {
		return err
	}
	for fragIdx, fr := range frames {
		if len(fr.Payload) > media.MaxFrameBytes {
			return media.NewError(media.ErrLimitExceeded, hdr.SourceID, "frame too large")
		}
		if err := writeMoofMdat(mw, hdr, uint32(fragIdx+1), fr); err != nil {
			return err
		}
	}
	return nil
}

func brand(s string) [4]byte {
	var b [4]byte
	copy(b[:], s)
	return b
}

// WriteFragmentedMP4Bytes renders frames to an in-memory fMP4 byte slice.
func WriteFragmentedMP4Bytes(hdr FSEQHeader, frames []*media.RawFrame) ([]byte, error) {
	var buf memBuffer
	if err := WriteFragmentedMP4(&buf, hdr, frames); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// memBuffer is an in-memory io.WriteSeeker with sparse-write support.
type memBuffer struct {
	data []byte
	pos  int64
}

func (m *memBuffer) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.data)) {
		grown := make([]byte, end)
		copy(grown, m.data)
		m.data = grown
	}
	copy(m.data[m.pos:end], p)
	m.pos = end
	return len(p), nil
}

func (m *memBuffer) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.pos = offset
	case io.SeekCurrent:
		m.pos += offset
	case io.SeekEnd:
		m.pos = int64(len(m.data)) + offset
	}
	if m.pos < 0 {
		return 0, errNegativeSeek
	}
	return m.pos, nil
}

func (m *memBuffer) Bytes() []byte { return m.data }

var errNegativeSeek = errors.New("negative seek position")

// writeMoov emits the init segment: one track, timescale 1_000_000 so each
// track time unit is one microsecond, plus mvex/trex signalling fragmentation.
func writeMoofMdat(mw *mp4.Writer, hdr FSEQHeader, seq uint32, fr *media.RawFrame) error {
	start := func(t mp4.BoxType) error { _, err := mw.StartBox(&mp4.BoxInfo{Type: t}); return err }
	end := mw.EndBox

	moofStart, err := mw.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if err := start(mp4.BoxTypeMoof()); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeMfhd()); err != nil {
		return err
	}
	if _, err := mp4.Marshal(mw, &mp4.Mfhd{SequenceNumber: seq}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := end(); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeTraf()); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeTfhd()); err != nil {
		return err
	}
	if _, err := mp4.Marshal(mw, &mp4.Tfhd{
		FullBox: mp4.FullBox{Flags: [3]byte{0x00, 0x00, 0x02}},
		TrackID: hdr.SourceID, SampleDescriptionIndex: 1,
	}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := end(); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeTfdt()); err != nil {
		return err
	}
	if _, err := mp4.Marshal(mw, &mp4.Tfdt{
		FullBox:               mp4.FullBox{Version: 1},
		BaseMediaDecodeTimeV1: fr.Meta.Sequence,
	}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := end(); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeTrun()); err != nil {
		return err
	}
	trun := &mp4.Trun{
		FullBox:     mp4.FullBox{Flags: [3]byte{0x00, 0x0f, 0x01}},
		SampleCount: 1,
		Entries: []mp4.TrunEntry{{
			SampleDuration: 1, SampleSize: uint32(len(fr.Payload)),
		}},
	}
	if _, err := mp4.Marshal(mw, trun, mp4.Context{}); err != nil {
		return err
	}
	trunBI, err := end()
	if err != nil {
		return err
	}
	// Private relay metadata: capture anchors lost by standard MP4 boxes.
	if err := start(rmtaBoxType()); err != nil {
		return err
	}
	rmta := encodeRMTA(rmtaPayload{
		Sequence:     fr.Meta.Sequence,
		ClockTicks:   fr.Meta.ClockTicks,
		CaptureRefNS: fr.Meta.CaptureRefNS,
		TimescaleHz:  uint32(fr.Meta.TimescaleHz),
	})
	if _, err := mw.Write(rmta); err != nil {
		return err
	}
	if _, err := end(); err != nil {
		return err
	}
	if _, err := end(); err != nil { // traf
		return err
	}
	moofBI, err := end()
	if err != nil {
		return err
	}
	_ = moofBI
	if err := start(mp4.BoxTypeMdat()); err != nil {
		return err
	}
	payloadPos, err := mw.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if _, err := mw.Write(fr.Payload); err != nil {
		return err
	}
	if _, err := end(); err != nil {
		return err
	}
	// Patch trun DataOffset (relative to moof start), then restore position.
	endPos, err := mw.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	trun.DataOffset = int32(payloadPos - moofStart)
	if _, err := trunBI.SeekToPayload(mw); err != nil {
		return err
	}
	if _, err := mp4.Marshal(mw, trun, mp4.Context{}); err != nil {
		return err
	}
	if _, err := mw.Seek(endPos, io.SeekStart); err != nil {
		return err
	}
	return nil
}

// hashBytes returns the SHA-256 digest of the given payload, matching the
// checksum convention used by every demuxer.
func hashBytes(p []byte) [32]byte { return sha256.Sum256(p) }
