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
}

type trafInfo struct {
	moofOff uint64
	trackID uint32
	tfdt    uint64
	dataOff int64
	durs    []uint32
	sizes   []uint32
	cts     []int64
}

// Demux parses one (f)MP4 segment. Truncated or malformed input
// yields a Fatal fault naming the segment.
func Demux(data []byte, name string) ([]Track, error) {
	var tracks []Track
	byID := map[uint32]*Track{}
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
			byID[cur.ID] = &tracks[len(tracks)-1]
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
			cur.Lang = string(m.Language[:])
		case mp4.BoxTypeHdlr():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			cur.Handler = string(box.(*mp4.Hdlr).HandlerType[:])
		case mp4.BoxTypeStsd():
			var buf bytes.Buffer
			if _, err := h.ReadData(&buf); err != nil {
				return nil, err
			}
			if b := buf.Bytes(); len(b) >= 16 {
				cur.Codec = string(b[12:16])
			}
		case mp4.BoxTypeTfhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			curTraf.trackID = box.(*mp4.Tfhd).TrackID
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
			return nil, fault.Newf(fault.Fatal, "demux", name, "truncated MP4")
		}
		return nil, fault.Wrap(fault.Fatal, "demux", name, err)
	}
	if len(tracks) == 0 {
		return nil, fault.Newf(fault.Fatal, "demux", name, "no tracks found")
	}
	if err := assemble(data, name, byID, trafs, mdats); err != nil {
		return nil, err
	}
	return tracks, nil
}

func assemble(data []byte, name string, byID map[uint32]*Track, trafs []*trafInfo, mdats []mp4.BoxInfo) error {
	for _, tf := range trafs {
		tr, ok := byID[tf.trackID]
		if !ok {
			return fault.Newf(fault.Fatal, "demux", name, "traf references unknown track %d", tf.trackID)
		}
		if len(tf.durs) != len(tf.sizes) {
			return fault.Newf(fault.Fatal, "demux", name, "inconsistent trun entries")
		}
		period := clock.WrapPeriod(tr.Timescale)
		abs := tf.moofOff + uint64(tf.dataOff)
		cum := uint64(0)
		for i := range tf.sizes {
			sz := uint64(tf.sizes[i])
			if abs+sz > uint64(len(data)) || !inAnyMdat(abs, sz, mdats) {
				return fault.Newf(fault.Fatal, "demux", name, "truncated MP4: sample data out of range")
			}
			tr.Samples = append(tr.Samples, Sample{
				DTS:    (tf.tfdt + cum) % period,
				Dur:    tf.durs[i],
				CTSOff: tf.cts[i],
				Data:   data[abs : abs+sz],
			})
			abs += sz
			cum += uint64(tf.durs[i])
		}
	}
	return nil
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
