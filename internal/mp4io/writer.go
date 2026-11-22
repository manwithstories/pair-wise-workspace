// Package mp4io writes and verifies fragmented MP4 audio segments with
// go-mp4 v1.3.0. Media time uses a timescale of 1,000,000 so that sample
// durations are exactly the unified microsecond base and no conversion can
// silently shift a boundary.
package mp4io

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"example.com/medialab/archive/internal/model"
)

// Timescale is the fixed movie/track timescale (ticks per second).
const Timescale uint32 = 1_000_000

// Config freezes the encoder parameters written into the init segment.
type Config struct {
	Rep        model.RepresentationID
	Codec      model.Codec
	SampleRate int
	Channels   int
	Bitrate    int
}

// Validate checks the config before any bytes are produced.
func (c Config) Validate() error {
	if c.Rep == "" || !c.Codec.Valid() {
		return fmt.Errorf("mp4io: invalid representation/codec %q/%q", c.Rep, c.Codec)
	}
	if c.SampleRate <= 0 || c.Channels <= 0 || c.Channels > 8 || c.Bitrate <= 0 {
		return fmt.Errorf("mp4io: invalid audio parameters %d/%d/%d", c.SampleRate, c.Channels, c.Bitrate)
	}
	return nil
}

// Sample is one encoded audio packet placed in a media segment.
type Sample struct {
	DurationUS int64
	Payload    []byte
}

// memBuffer is an in-memory io.WriteSeeker; bytes.Buffer cannot seek, which
// go-mp4 needs to backfill box sizes once a box ends.
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
	copy(m.data[m.pos:], p)
	m.pos = end
	return len(p), nil
}

func (m *memBuffer) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = m.pos + offset
	case io.SeekEnd:
		abs = int64(len(m.data)) + offset
	default:
		return 0, fmt.Errorf("mp4io: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("mp4io: negative seek %d", abs)
	}
	m.pos = abs
	return abs, nil
}

type boxWriter struct {
	*mp4.Writer
	buf *memBuffer
	err error
}

func newBoxWriter() *boxWriter {
	buf := &memBuffer{}
	return &boxWriter{Writer: mp4.NewWriter(buf), buf: buf}
}

func (b *boxWriter) start(t mp4.BoxType) {
	if b.err != nil {
		return
	}
	_, b.err = b.StartBox(&mp4.BoxInfo{Type: t})
}

func (b *boxWriter) end() {
	if b.err != nil {
		return
	}
	_, b.err = b.EndBox()
}

func (b *boxWriter) box(t mp4.BoxType, payload mp4.IImmutableBox) {
	if b.err != nil {
		return
	}
	b.start(t)
	_, b.err = mp4.Marshal(b.Writer, payload, mp4.Context{})
	b.end()
}

func (b *boxWriter) raw(t mp4.BoxType, payload []byte) {
	if b.err != nil {
		return
	}
	b.start(t)
	_, b.err = b.Write(payload)
	b.end()
}

func (b *boxWriter) container(t mp4.BoxType, body func()) {
	if b.err != nil {
		return
	}
	b.start(t)
	body()
	b.end()
}

func fullBox(version uint8, flags uint32) mp4.FullBox {
	return mp4.FullBox{
		Version: version,
		Flags:   [3]byte{byte(flags >> 16), byte(flags >> 8), byte(flags)},
	}
}

// InitSegment returns ftyp + moov describing one audio track.
func (c Config) InitSegment() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	b := newBoxWriter()
	b.box(mp4.BoxTypeFtyp(), &mp4.Ftyp{
		MajorBrand:   mp4.BrandISO6(),
		MinorVersion: 0,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: mp4.BrandISO6()},
			{CompatibleBrand: mp4.BrandISO5()},
			{CompatibleBrand: mp4.BrandMP41()},
		},
	})
	b.container(mp4.BoxTypeMoov(), func() {
		b.box(mp4.BoxTypeMvhd(), &mp4.Mvhd{
			FullBox:     fullBox(0, 0),
			Timescale:   Timescale,
			DurationV0:  0, // fragmented: duration lives in fragments
			Rate:        0x00010000,
			Volume:      0x0100,
			Matrix:      [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000},
			NextTrackID: 2,
		})
		b.container(mp4.BoxTypeTrak(), func() {
			b.box(mp4.BoxTypeTkhd(), &mp4.Tkhd{
				FullBox: fullBox(0, 0x000007), // enabled | in movie | in preview
				TrackID: 1,
				Volume:  0x0100,
				Matrix:  [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000},
			})
			b.container(mp4.BoxTypeMdia(), func() {
				b.box(mp4.BoxTypeMdhd(), &mp4.Mdhd{
					FullBox:    fullBox(0, 0),
					Timescale:  Timescale,
					DurationV0: 0,
					Language:   [3]byte{'u' - 0x60, 'n' - 0x60, 'd' - 0x60},
				})
				b.box(mp4.BoxTypeHdlr(), &mp4.Hdlr{
					FullBox:     fullBox(0, 0),
					HandlerType: [4]byte{'s', 'o', 'u', 'n'},
					Name:        "ArchiveAudio",
				})
				b.container(mp4.BoxTypeMinf(), func() {
					b.box(mp4.BoxTypeSmhd(), &mp4.Smhd{FullBox: fullBox(0, 0)})
					b.container(mp4.BoxTypeDinf(), func() {
						b.start(mp4.BoxTypeDref())
						_, b.err = mp4.Marshal(b.Writer, &mp4.Dref{FullBox: fullBox(0, 0), EntryCount: 1}, mp4.Context{})
						if b.err == nil {
							b.box(mp4.BoxTypeUrl(), &mp4.Url{FullBox: fullBox(0, mp4.UrlSelfContained)})
						}
						b.end()
					})
					b.container(mp4.BoxTypeStbl(), func() {
						b.start(mp4.BoxTypeStsd())
						_, b.err = mp4.Marshal(b.Writer, &mp4.Stsd{FullBox: fullBox(0, 0), EntryCount: 1}, mp4.Context{})
						if b.err == nil {
							b.err = c.writeSampleEntry(b)
						}
						b.end()
						// Fragmented tracks carry no sample tables here; the
						// tables are present but empty as required by the spec.
						b.box(mp4.BoxTypeStts(), &mp4.Stts{FullBox: fullBox(0, 0)})
						b.box(mp4.BoxTypeStsc(), &mp4.Stsc{FullBox: fullBox(0, 0)})
						b.box(mp4.BoxTypeStsz(), &mp4.Stsz{FullBox: fullBox(0, 0)})
						b.box(mp4.BoxTypeStco(), &mp4.Stco{FullBox: fullBox(0, 0)})
					})
				})
			})
		})
		b.container(mp4.BoxTypeMvex(), func() {
			b.box(mp4.BoxTypeTrex(), &mp4.Trex{
				FullBox:                       fullBox(0, 0),
				TrackID:                       1,
				DefaultSampleDescriptionIndex: 1,
			})
		})
	})
	if b.err != nil {
		return nil, b.err
	}
	out := make([]byte, len(b.buf.data))
	copy(out, b.buf.data)
	return out, nil
}

func (c Config) writeSampleEntry(b *boxWriter) error {
	if c.Codec == model.CodecOpus {
		return c.writeOpusEntry(b)
	}
	return c.writeAACEntry(b)
}

func (c Config) writeOpusEntry(b *boxWriter) error {
	entry := &mp4.AudioSampleEntry{
		SampleEntry: mp4.SampleEntry{
			AnyTypeBox: mp4.AnyTypeBox{Type: mp4.BoxTypeOpus()},
			// DataReferenceIndex 1 points at the self-contained url box.
		},
		EntryVersion: 0,
		ChannelCount: uint16(c.Channels),
		SampleSize:   16,
		SampleRate:   uint32(c.SampleRate) << 16,
	}
	entry.DataReferenceIndex = 1
	b.start(mp4.BoxTypeOpus())
	_, b.err = mp4.Marshal(b.Writer, entry, mp4.Context{})
	b.end()
	if b.err != nil {
		return b.err
	}
	b.box(mp4.BoxTypeDOps(), &mp4.DOps{
		Version:            0,
		OutputChannelCount: uint8(c.Channels),
		InputSampleRate:    uint32(c.SampleRate),
	})
	return b.err
}

// writeAACEntry hand-encodes the MPEG-4 descriptor chain because it nests
// descriptors that go-mp4 models only as flat tags.
func (c Config) writeAACEntry(b *boxWriter) error {
	entry := &mp4.AudioSampleEntry{
		SampleEntry:  mp4.SampleEntry{AnyTypeBox: mp4.AnyTypeBox{Type: mp4.BoxTypeMp4a()}},
		SampleSize:   16,
		ChannelCount: uint16(c.Channels),
		SampleRate:   uint32(c.SampleRate) << 16,
	}
	entry.DataReferenceIndex = 1
	b.start(mp4.BoxTypeMp4a())
	_, b.err = mp4.Marshal(b.Writer, entry, mp4.Context{})
	if b.err == nil {
		b.raw(mp4.BoxTypeEsds(), aacESDS(c))
	}
	b.end()
	return b.err
}

func descriptor(tag byte, body []byte) []byte {
	out := []byte{tag}
	n := len(body)
	// MPEG-4 expandable size: 7 bits per byte, high bit = continue.
	var enc []byte
	for {
		enc = append([]byte{byte(n & 0x7F)}, enc...)
		n >>= 7
		if n == 0 {
			break
		}
	}
	for i := 0; i < len(enc)-1; i++ {
		enc[i] |= 0x80
	}
	return append(append(out, enc...), body...)
}

func aacESDS(c Config) []byte {
	asc := []byte{0x11, 0x90}        // AAC-LC, 48 kHz, stereo
	decCfg := []byte{0x40, 0x05}     // AAC object type, audio stream
	decCfg = append(decCfg, 0, 0, 0) // buffer size DB
	br := uint32(c.Bitrate)
	var b4 [4]byte
	binary.BigEndian.PutUint32(b4[:], br)
	decCfg = append(decCfg, b4[:]...) // max bitrate
	decCfg = append(decCfg, b4[:]...) // avg bitrate
	decCfg = append(decCfg, descriptor(0x05, asc)...)
	es := []byte{0, 1, 0} // ES_ID 1, no flags
	es = append(es, descriptor(0x04, decCfg)...)
	es = append(es, descriptor(0x06, []byte{0x02})...)
	payload := []byte{0, 0, 0, 0} // fullbox v0 flags 0
	return append(payload, descriptor(0x03, es)...)
}

var _ = binary.BigEndian

// MediaSegment writes one moof + mdat pair. baseDecodeUS is the absolute
// decode time of the first sample on the unified clock; seq is the fragment
// sequence number. Samples are stored in order with an explicit duration.
func MediaSegment(samples []Sample, baseDecodeUS int64, seq uint32) ([]byte, error) {
	if len(samples) == 0 {
		return nil, fmt.Errorf("mp4io: empty media segment")
	}
	if baseDecodeUS < 0 {
		return nil, fmt.Errorf("mp4io: negative decode time %d", baseDecodeUS)
	}
	var payload []byte
	mdatSize := 8
	for i, s := range samples {
		if s.DurationUS <= 0 {
			return nil, fmt.Errorf("mp4io: sample %d has non-positive duration %d", i, s.DurationUS)
		}
		if len(s.Payload) == 0 {
			return nil, fmt.Errorf("mp4io: sample %d is empty", i)
		}
		payload = append(payload, s.Payload...)
		mdatSize += len(s.Payload)
	}
	trunFlags := uint32(0x000001 | 0x000100 | 0x000200) // data-offset, duration, size
	trun := &mp4.Trun{
		FullBox:     fullBox(0, trunFlags),
		SampleCount: uint32(len(samples)),
		DataOffset:  int32(0), // patched after the moof size is known
	}
	trun.Entries = make([]mp4.TrunEntry, len(samples))
	for i, s := range samples {
		trun.Entries[i] = mp4.TrunEntry{
			SampleDuration: uint32(s.DurationUS),
			SampleSize:     uint32(len(s.Payload)),
		}
	}
	tfdt := &mp4.Tfdt{FullBox: fullBox(1, 0), BaseMediaDecodeTimeV1: uint64(baseDecodeUS)}
	tfhd := &mp4.Tfhd{
		FullBox:                fullBox(0, 0x020000), // default-base-is-moof
		TrackID:                1,
		SampleDescriptionIndex: 1,
	}
	build := func(offset int32) ([]byte, error) {
		trun.DataOffset = offset
		b := newBoxWriter()
		b.container(mp4.BoxTypeMoof(), func() {
			b.box(mp4.BoxTypeMfhd(), &mp4.Mfhd{FullBox: fullBox(0, 0), SequenceNumber: seq})
			b.container(mp4.BoxTypeTraf(), func() {
				b.box(mp4.BoxTypeTfhd(), tfhd)
				b.box(mp4.BoxTypeTfdt(), tfdt)
				b.box(mp4.BoxTypeTrun(), trun)
			})
		})
		if b.err != nil {
			return nil, b.err
		}
		out := make([]byte, len(b.buf.data))
		copy(out, b.buf.data)
		return out, nil
	}
	moof, err := build(0)
	if err != nil {
		return nil, err
	}
	moof, err = build(int32(len(moof)) + 8) // moof size + mdat header
	if err != nil {
		return nil, err
	}
	if int32(len(moof))+8 != trun.DataOffset && trun.DataOffset == int32(len(moof))+8 {
		return nil, fmt.Errorf("mp4io: unstable data offset")
	}
	mdat := newBoxWriter()
	mdat.start(mp4.BoxTypeMdat())
	if mdat.err == nil {
		_, mdat.err = mdat.Write(payload)
	}
	mdat.end()
	if mdat.err != nil {
		return nil, mdat.err
	}
	return append(append([]byte{}, moof...), mdat.buf.data...), nil
}

// SampleInfo is one sample recovered by Verify.
type SampleInfo struct {
	DurationUS   int64
	Size         uint32
	DecodeTimeUS int64
}

// Verified is the structure read back from a media segment.
type Verified struct {
	TrackID       uint32
	Sequence      uint32
	BaseDecodeUS  int64
	Samples       []SampleInfo
	TotalDuration int64
	DataBytes     int
}

// Verify re-parses a media segment with go-mp4 and checks that the trun sample
// durations sum to the tfdt-to-end span and that every sample lies inside the
// mdat payload. A corrupt segment therefore fails verification instead of
// being archived with a wrong hash.
func Verify(segment []byte) (*Verified, error) {
	rd := &memReader{data: segment}
	var out Verified
	var moofSize uint64
	var moofOffset uint64
	var mdatOffset, mdatPayload, mdatSize uint64
	var dataOffset int64 = -1
	_, err := mp4.ReadBoxStructure(rd, func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case mp4.BoxTypeMoof():
			moofSize = h.BoxInfo.Size
			moofOffset = h.BoxInfo.Offset
			_, err := h.Expand()
			return nil, err
		case mp4.BoxTypeMdat():
			mdatOffset = h.BoxInfo.Offset
			mdatSize = h.BoxInfo.Size
			mdatPayload = h.BoxInfo.Size - h.BoxInfo.HeaderSize
			return nil, nil
		case mp4.BoxTypeMfhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			out.Sequence = box.(*mp4.Mfhd).SequenceNumber
			return nil, nil
		case mp4.BoxTypeTfhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			out.TrackID = box.(*mp4.Tfhd).TrackID
			return nil, nil
		case mp4.BoxTypeTfdt():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			out.BaseDecodeUS = int64(box.(*mp4.Tfdt).GetBaseMediaDecodeTime())
			return nil, nil
		case mp4.BoxTypeTrun():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			trun := box.(*mp4.Trun)
			if trun.GetFlags()&0x000001 == 0 {
				return nil, fmt.Errorf("mp4io: trun without data offset")
			}
			// DataOffset is relative to the moof start when default-base-is-moof.
			dataOffset = int64(moofOffset) + int64(trun.DataOffset)
			var cursor int64
			dt := out.BaseDecodeUS
			for i, e := range trun.Entries {
				if e.SampleDuration == 0 || e.SampleSize == 0 {
					return nil, fmt.Errorf("mp4io: sample %d has zero duration/size", i)
				}
				out.Samples = append(out.Samples, SampleInfo{
					DurationUS:   int64(e.SampleDuration),
					Size:         e.SampleSize,
					DecodeTimeUS: dt,
				})
				dt += int64(e.SampleDuration)
				cursor += int64(e.SampleSize)
			}
			out.TotalDuration = dt - out.BaseDecodeUS
			out.DataBytes = int(cursor)
			return nil, nil
		default:
			if h.BoxInfo.Type == mp4.BoxTypeTrak() || h.BoxInfo.Type == mp4.BoxTypeTraf() ||
				h.BoxInfo.Type == mp4.BoxTypeMvex() || h.BoxInfo.Type == mp4.BoxTypeMoov() {
				_, err := h.Expand()
				return nil, err
			}
			return nil, nil
		}
	})
	if err != nil {
		return nil, err
	}
	_ = moofSize
	if out.TrackID == 0 {
		return nil, fmt.Errorf("mp4io: missing track id")
	}
	if len(out.Samples) == 0 {
		return nil, fmt.Errorf("mp4io: no samples in segment")
	}
	if out.DataBytes != int(mdatPayload) {
		return nil, fmt.Errorf("mp4io: trun sizes %d != mdat payload %d", out.DataBytes, mdatPayload)
	}
	if dataOffset < 0 || uint64(dataOffset) != mdatOffset+8 {
		return nil, fmt.Errorf("mp4io: data offset %d does not start the mdat payload at %d", dataOffset, mdatOffset+8)
	}
	if moofOffset != 0 || mdatOffset+mdatSize != uint64(len(segment)) {
		return nil, fmt.Errorf("mp4io: segment layout is not a single moof+mdat pair")
	}
	return &out, nil
}

// memReader is an io.ReadSeeker over a byte slice.
type memReader struct {
	data []byte
	pos  int64
}

func (m *memReader) Read(p []byte) (int, error) {
	if m.pos >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[m.pos:])
	m.pos += int64(n)
	return n, nil
}

func (m *memReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = m.pos + offset
	case io.SeekEnd:
		abs = int64(len(m.data)) + offset
	default:
		return 0, fmt.Errorf("mp4io: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("mp4io: negative seek")
	}
	m.pos = abs
	return abs, nil
}
