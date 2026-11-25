// Package packager writes fragmented MP4 (fMP4) using the abema/go-mp4 box
// writer. Video is H.264 (avc1/avcC); the private PCM16LE audio track uses a
// custom sample-entry box type rather than pretending to be a supported codec.
package packager

import (
	"encoding/binary"
	"io"

	mp4 "github.com/abema/go-mp4"

	"streamcut/internal/media"
)

// TrackConfig describes one fMP4 track.
type TrackConfig struct {
	TrackID    uint32
	Timescale  uint32
	Video      *VideoConfig
	Audio      *AudioConfig
}

// VideoConfig carries the SPS/PPS NAL units (Annex B) for the avcC box.
type VideoConfig struct {
	Width, Height uint16
	SPS, PPS      []byte // raw NAL bytes without start code
}

// AudioConfig describes the private PCM16LE little-endian mono track.
type AudioConfig struct {
	SampleRate uint32
	Channels   uint16
}

const (
	// videoTrackID / audioTrackID are fixed for every generated stream.
	videoTrackID uint32 = 1
	audioTrackID uint32 = 2
	// pcmCodec is the private sample-entry fourCC for PCM16LE.
	pcmCodec = "pcml"
)

// WriteInitSegment writes the fMP4 initialization segment: ftyp + a moov with
// movie/track headers, an avc1 sample entry (video) or a private pcml entry
// (audio), and trex defaults for fragmentation.
func WriteInitSegment(w io.WriteSeeker, tracks []TrackConfig) error {
	mw := mp4.NewWriter(w)
	if err := writeFtyp(mw); err != nil {
		return err
	}
	_, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()})
	if err != nil {
		return err
	}
	if err := writeMvhd(mw); err != nil {
		return err
	}
	for _, tc := range tracks {
		if err := writeTrak(mw, tc); err != nil {
			return err
		}
	}
	return endBox(mw)
}

func writeFtyp(mw *mp4.Writer) error {
	_, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()})
	if err != nil {
		return err
	}
	ftyp := &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 0x200,
	}
	ftyp.AddCompatibleBrand([4]byte{'i', 's', 'o', 'm'})
	ftyp.AddCompatibleBrand([4]byte{'i', 's', 'o', '5'})
	if _, err := mp4.Marshal(mw, ftyp, mp4.Context{}); err != nil {
		return err
	}
	return endBox(mw)
}

func writeMvhd(mw *mp4.Writer) error {
	_, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMvhd()})
	if err != nil {
		return err
	}
	mvhd := &mp4.Mvhd{
		FullBox:     mp4.FullBox{Version: 0},
		Timescale:   90000,
		Rate:        0x00010000,
		Volume:      0x0100,
		NextTrackID: 3,
		Matrix:      matrixIdentity,
	}
	if _, err := mp4.Marshal(mw, mvhd, mp4.Context{}); err != nil {
		return err
	}
	return endBox(mw)
}

// Sample is one mdat entry: payload plus duration in track timescale ticks.
type Sample struct {
	Data     []byte
	Duration uint32
	IsSync   bool
}

// MediaSegment is one fragmented movie: a single moof/mdat pair per track set.
type MediaSegment struct {
	Sequence uint32
	Samples  map[uint32][]Sample // track ID -> samples
}

// trafSize predicts the serialized size of one traf box. Layout is fixed:
// traf header(8) + tfhd(8 header + 8 payload) + trun(8 header + 12 fixed
// payload + 16 bytes per entry).
func trafSize(n int) int64 { return 8 + 16 + 20 + int64(n)*16 }

// WriteMediaSegment writes one self-contained moof+mdat fragment. All tracks
// share one moof with one traf each, matching the segmenter's cut boundaries.
// Each trun's data_offset is precomputed from the deterministic box sizes so
// it points at that track's first sample inside mdat, relative to moof start.
func WriteMediaSegment(w io.WriteSeeker, seg MediaSegment) error {
	mw := mp4.NewWriter(w)
	order := make([]uint32, 0, len(seg.Samples))
	if _, ok := seg.Samples[videoTrackID]; ok {
		order = append(order, videoTrackID)
	}
	if _, ok := seg.Samples[audioTrackID]; ok {
		order = append(order, audioTrackID)
	}
	// moof = moof header(8) + mfhd(16) + one traf per track.
	moofSize := int64(8) + 16
	for _, tid := range order {
		moofSize += trafSize(len(seg.Samples[tid]))
	}
	mdatHeader := int64(8)

	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoof()}); err != nil {
		return err
	}
	if err := writeMfhd(mw, seg.Sequence); err != nil {
		return err
	}
	cumulative := mdatHeader
	for _, tid := range order {
		offset := moofSize + cumulative
		if err := writeTraf(mw, tid, seg.Samples[tid], offset); err != nil {
			return err
		}
		for _, s := range seg.Samples[tid] {
			cumulative += int64(len(s.Data))
		}
	}
	if err := endBox(mw); err != nil { // moof
		return err
	}
	// mdat with all sample payloads in the same track order as the trafs.
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return err
	}
	for _, tid := range order {
		for _, s := range seg.Samples[tid] {
			if _, err := mw.Write(s.Data); err != nil {
				return err
			}
		}
	}
	if err := endBox(mw); err != nil {
		return err
	}
	return nil
}

// EncodeToBytes runs a writer callback against an in-memory seekable buffer.
func EncodeToBytes(fn func(io.WriteSeeker) error) ([]byte, error) {
	ws := newMemWriteSeeker()
	if err := fn(ws); err != nil {
		return nil, err
	}
	return ws.bytes(), nil
}

// memWriteSeeker is a preallocated, random-access byte buffer. A plain
// bytes.Buffer cannot satisfy io.WriteSeeker: EndBox seeks back to patch box
// sizes, which requires overwriting earlier bytes rather than appending. The
// backing slice grows on demand and tracks the highest written position.
type memWriteSeeker struct {
	data []byte
	pos  int64
	end  int64
}

func newMemWriteSeeker() *memWriteSeeker {
	return &memWriteSeeker{data: make([]byte, 0, 4096)}
}

func (m *memWriteSeeker) Write(p []byte) (int, error) {
	need := m.pos + int64(len(p))
	if int64(cap(m.data)) < need {
		grown := make([]byte, need, need*2)
		copy(grown, m.data[:m.end])
		m.data = grown
	}
	m.data = m.data[:need]
	n := copy(m.data[m.pos:], p)
	m.pos += int64(n)
	if m.pos > m.end {
		m.end = m.pos
	}
	return n, nil
}

func (m *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.pos = offset
	case io.SeekCurrent:
		m.pos += offset
	default:
		m.pos = m.end + offset
	}
	return m.pos, nil
}

func (m *memWriteSeeker) bytes() []byte { return m.data[:m.end] }

// u16/u32 helpers keep the custom-box payload construction terse.
func u16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }
func u32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }

// StreamKindToSampleEntry reports whether a media kind has a packaging path.
func StreamKindToSampleEntry(k media.StreamKind) bool {
	return k == media.KindVideoH264 || k == media.KindAudioPCM16LE
}
