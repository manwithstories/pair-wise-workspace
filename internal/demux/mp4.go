// Package demux extracts raw samples and timestamps from MP4/fMP4
// segments using go-mp4, sharing the clock and fault models.
package demux

import (
	"bytes"
	"io"

	"github.com/abema/go-mp4"

	"archive/internal/clock"
	"archive/internal/fault"
)

// Sample is one raw access unit with its wrapped source timestamp.
type Sample struct {
	DTS    uint64 // raw decode time mod wrap period, track timescale
	Dur    uint32 // duration, track timescale
	CTSOff int64  // composition offset, track timescale
	Data   []byte
}

// Track carries demuxed samples plus identity used for mapping.
type Track struct {
	ID        uint32
	Timescale uint32
	Lang      string
	Codec     string // sample entry fourcc
	Handler   string // "vide", "soun", ...
	Samples   []Sample
	// Entry is the raw first sample entry from stsd (box header plus
	// payload, including avcC/esds etc.), copied verbatim so a remux
	// stays lossless without re-deriving decoder configuration.
	Entry []byte
	// Width/Height in pixels for visual tracks.
	Width, Height uint16
	// Channels, SampleBits and SampleRate for audio tracks.
	Channels   uint16
	SampleBits uint16
	SampleRate uint32
}

type trafInfo struct {
	moofOff uint64
	trackID uint32
	tfdt    uint64
	base    uint64 // tfhd base_data_offset, nilBase when absent
	nilBase bool
	dataOff int64
	durs    []uint32
	sizes   []uint32
	cts     []int64
}

// Demux parses one (f)MP4 segment. Truncated sample data is Degraded:
// the tracks parsed so far are returned with the error so a caller can
// still recover the complete prefix. Structural faults are Fatal.
func Demux(data []byte, name string) ([]Track, error) {
	var tracks []Track
	// byID stores slice indices, not pointers: appending the next trak
	// may reallocate the backing array and invalidate pointers.
	byID := map[uint32]int{}
	var cur *Track
	var trafs []*trafInfo
	var curTraf *trafInfo
	var moofOff uint64
	var mdats []mp4.BoxInfo

	_, err := mp4.ReadBoxStructure(bytes.NewReader(data), func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case mp4.BoxTypeMoov(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl():
			return h.Expand()
		case mp4.BoxTypeTrak():
			cur = &Track{}
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
			tracks = append(tracks, *cur)
			byID[cur.ID] = len(tracks) - 1
			cur = nil
			return nil, nil
		case mp4.BoxTypeMoof():
			moofOff = h.BoxInfo.Offset
			return h.Expand()
		case mp4.BoxTypeTraf():
			curTraf = &trafInfo{moofOff: moofOff}
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
			trafs = append(trafs, curTraf)
			curTraf = nil
			return nil, nil
		case mp4.BoxTypeTkhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			cur.ID = box.(*mp4.Tkhd).TrackID
		case mp4.BoxTypeMdhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			m := box.(*mp4.Mdhd)
			cur.Timescale = m.Timescale
			cur.Lang = decodeLang(m.Language)
		case mp4.BoxTypeHdlr():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			cur.Handler = string(box.(*mp4.Hdlr).HandlerType[:])
		case mp4.BoxTypeStsd():
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
			return nil, nil
		case mp4.BoxTypeAvc1(), mp4.BoxTypeHev1(), mp4.BoxTypeHvc1(),
			mp4.BoxTypeVp08(), mp4.BoxTypeVp09(), mp4.BoxTypeAv01(), mp4.BoxTypeMp4v():
			if cur == nil || cur.Codec != "" {
				return nil, nil
			}
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			v := box.(*mp4.VisualSampleEntry)
			cur.Codec = h.BoxInfo.Type.String()
			cur.Width, cur.Height = v.Width, v.Height
			cur.Entry = boxBytes(data, h.BoxInfo)
			return nil, nil
		case mp4.BoxTypeMp4a(), mp4.BoxTypeOpus():
			if cur == nil || cur.Codec != "" {
				return nil, nil
			}
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			a := box.(*mp4.AudioSampleEntry)
			cur.Codec = h.BoxInfo.Type.String()
			cur.Channels, cur.SampleBits = a.ChannelCount, a.SampleSize
			cur.SampleRate = a.SampleRate >> 16
			cur.Entry = boxBytes(data, h.BoxInfo)
			return nil, nil
		case mp4.BoxTypeTfhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			t := box.(*mp4.Tfhd)
			curTraf.trackID = t.TrackID
			if t.CheckFlag(mp4.TfhdBaseDataOffsetPresent) {
				curTraf.base = t.BaseDataOffset
			} else {
				curTraf.nilBase = true
			}
		case mp4.BoxTypeTfdt():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			curTraf.tfdt = box.(*mp4.Tfdt).GetBaseMediaDecodeTime()
		case mp4.BoxTypeTrun():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			tr := box.(*mp4.Trun)
			curTraf.dataOff = int64(tr.DataOffset)
			for _, e := range tr.Entries {
				curTraf.durs = append(curTraf.durs, e.SampleDuration)
				curTraf.sizes = append(curTraf.sizes, e.SampleSize)
				curTraf.cts = append(curTraf.cts, int64(tr.GetSampleCompositionTimeOffset(len(curTraf.cts))))
			}
		case mp4.BoxTypeMdat():
			mdats = append(mdats, h.BoxInfo)
		}
		return nil, nil
	})
	if err != nil {
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			return tracks, fault.Newf(fault.Degraded, "demux", name, "truncated MP4")
		}
		return tracks, fault.Wrap(fault.Fatal, "demux", name, err)
	}
	if len(tracks) == 0 {
		return nil, fault.Newf(fault.Fatal, "demux", name, "no tracks found")
	}
	if err := assemble(data, name, tracks, byID, trafs, mdats); err != nil {
		return tracks, err
	}
	return tracks, nil
}

func assemble(data []byte, name string, tracks []Track, byID map[uint32]int, trafs []*trafInfo, mdats []mp4.BoxInfo) error {
	for _, tf := range trafs {
		ti, ok := byID[tf.trackID]
		if !ok {
			return fault.Newf(fault.Fatal, "demux", name, "traf references unknown track %d", tf.trackID)
		}
		tr := &tracks[ti]
		if len(tf.durs) != len(tf.sizes) {
			return fault.Newf(fault.Fatal, "demux", name, "inconsistent trun entries")
		}
		if tr.Timescale == 0 {
			return fault.Newf(fault.Fatal, "demux", name, "track %d has zero timescale", tf.trackID)
		}
		period := clock.WrapPeriod(tr.Timescale)
		// Data offsets are signed relative to the base. Without an
		// explicit base_data_offset the base is the enclosing moof
		// (default-base-is-moof, or the implicit default).
		abs := int64(tf.moofOff) + tf.dataOff
		if !tf.nilBase {
			abs = int64(tf.base) + tf.dataOff
		}
		if abs < 0 {
			return fault.Newf(fault.Fatal, "demux", name, "negative sample offset")
		}
		cum := uint64(0)
		for i := range tf.sizes {
			sz := uint64(tf.sizes[i])
			off := uint64(abs)
			if off+sz > uint64(len(data)) || !inAnyMdat(off, sz, mdats) {
				return fault.Newf(fault.Degraded, "demux", name, "truncated MP4: sample data out of range")
			}
			tr.Samples = append(tr.Samples, Sample{
				DTS:    (tf.tfdt + cum) % period,
				Dur:    tf.durs[i],
				CTSOff: tf.cts[i],
				Data:   data[off : off+sz],
			})
			abs += int64(sz)
			cum += uint64(tf.durs[i])
		}
	}
	return nil
}

// decodeLang expands the packed ISO-639-2/T 5-bit form (char - 0x60)
// into a three-letter code such as "eng". Undefined codes become "".
func decodeLang(l [3]byte) string {
	var b [3]byte
	for i, c := range l {
		if c&0x1f == 0 {
			return ""
		}
		b[i] = c&0x1f + 0x60
	}
	return string(b[:])
}

// boxBytes returns the full raw box (header and payload) from data.
func boxBytes(data []byte, bi mp4.BoxInfo) []byte {
	end := bi.Offset + bi.Size
	if end > uint64(len(data)) {
		return nil
	}
	out := make([]byte, bi.Size)
	copy(out, data[bi.Offset:end])
	return out
}

func inAnyMdat(off, sz uint64, mdats []mp4.BoxInfo) bool {
	for _, m := range mdats {
		start := m.Offset + m.HeaderSize
		if off >= start && off+sz <= m.Offset+m.Size {
			return true
		}
	}
	return false
}
