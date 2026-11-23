package ingest

import (
	"encoding/binary"
	"io"

	mp4 "github.com/abema/go-mp4"

	"github.com/example/mediad/internal/model"
)

// parseState carries per-track context while walking the box tree.
type parseState struct {
	summary      *Summary
	track        *Track // current trak
	video        *VideoTrackSummary
	audio        *AudioTrackLayout
	containerRot Rotation
	movieColor   *Color
	pendingTrackID uint32
}

func containsBox(path []string, name string) bool {
	for _, p := range path {
		if p == name {
			return true
		}
	}
	return false
}

// validate enforces structural invariants the box walker alone cannot
// detect: a usable file needs at least one track whose sample table was
// fully walked. Truncated files yield zero tracks here.
func (s *Summary) validate(path string) error {
	if len(s.Tracks) == 0 {
		return model.New(model.KindCorrupt, "ingest", "mp4-parse",
			path+": no playable tracks (truncated or unreadable sample table)")
	}
	for _, tr := range s.Tracks {
		switch {
		case tr.Kind == "video" && (tr.Video == nil || tr.Video.SampleCount == 0):
			return model.New(model.KindCorrupt, "ingest", "mp4-parse",
				path+": video track has empty sample table")
		case tr.Kind == "audio" && (tr.Audio == nil || tr.Audio.SampleCount == 0):
			return model.New(model.KindCorrupt, "ingest", "mp4-parse",
				path+": audio track has empty sample table")
		}
	}
	return nil
}

// ParseMP4 parses an MP4 file into a normalized Summary. Only metadata
// boxes are unmarshalled; media bytes stay on disk, keeping per-segment
// memory bounded.
func ParseMP4(r io.ReadSeeker, path string) (*Summary, error) {
	st := &parseState{summary: &Summary{Format: "mp4", Path: path}}

	handler := func(h *mp4.ReadHandle) (interface{}, error) {
		p := boxTypes(h.Path)
		last := p[len(p)-1]
		// Container boxes must be expanded explicitly; the walker does
		// not recurse on its own.
		switch last {
		case "moov", "trak", "mdia", "minf", "stbl", "stsd", "dinf":
			return h.Expand()
		}
		inStbl := containsBox(p, "stbl")
		switch {
		case len(p) == 2 && last == "mvhd":
			return st.readMvhd(h)
		case len(p) == 3 && last == "tkhd":
			return st.startTrack(h)
		case len(p) == 4 && last == "mdhd":
			return st.readMdhd(h)
		case len(p) == 4 && last == "hdlr":
			return st.readHdlr(h)
		// trak-level descriptors (container-scope per our fixture layout)
		case len(p) == 3 && last == "clap":
			return st.readTrakClap(h)
		case len(p) == 3 && last == "colr":
			return st.readTrakColr(h)
		case len(p) == 2 && last == "colr":
			// moov-level container color description
			return st.readMovieColr(h)
		// sample-table / sample-entry descriptors (track-scope)
		case inStbl && last == "stts":
			return st.readStts(h)
		case inStbl && last == "stsz":
			return st.readStsz(h)
		case inStbl && last == "stss":
			return st.readStss(h)
		case inStbl && last == "colr":
			return st.readStsdColr(h)
		case inStbl && last == "clap":
			return st.readStsdClap(h)
		case inStbl && last == "avc1":
			if _, err := st.readAvc1(h); err != nil {
				return nil, err
			}
			return h.Expand()
		case inStbl && last == "mp4a":
			if _, err := st.readMpa4(h); err != nil {
				return nil, err
			}
			return h.Expand()
		case inStbl && last == "chnl":
			return st.readChnl(h)
		}
		return nil, nil
	}

	if _, err := mp4.ReadBoxStructure(r, handler); err != nil {
		return nil, model.Wrap(model.KindCorrupt, "ingest", "mp4-parse",
			path, err)
	}
	if err := st.summary.validate(path); err != nil {
		return nil, err
	}
	return st.summary, nil
}

func boxTypes(path mp4.BoxPath) []string {
	out := make([]string, len(path))
	for i, t := range path {
		out[i] = t.String()
	}
	return out
}

func (st *parseState) readMvhd(h *mp4.ReadHandle) (interface{}, error) {
	box, _, err := h.ReadPayload()
	if err != nil {
		return nil, err
	}
	mvhd := box.(*mp4.Mvhd)
	st.summary.Timescale = mvhd.Timescale
	st.containerRot = rotationFromMatrix(mvhd.Matrix)
	return nil, nil
}

func (st *parseState) startTrack(h *mp4.ReadHandle) (interface{}, error) {
	box, _, err := h.ReadPayload()
	if err != nil {
		return nil, err
	}
	tkhd := box.(*mp4.Tkhd)
	st.track = &Track{
		ContainerRot: st.containerRot,
		TrackRot:     rotationFromMatrix(tkhd.Matrix),
	}
	st.pendingTrackID = tkhd.TrackID
	st.video = nil
	st.audio = nil
	return nil, nil
}

func (st *parseState) readMdhd(h *mp4.ReadHandle) (interface{}, error) {
	box, _, err := h.ReadPayload()
	if err != nil {
		return nil, err
	}
	mdhd := box.(*mp4.Mdhd)
	if mdhd.Timescale == 0 {
		return nil, model.New(model.KindCorrupt, "ingest", "mp4-parse",
			"mdhd declares zero timescale")
	}
	if st.video != nil {
		st.video.Timescale = mdhd.Timescale
	}
	if st.audio != nil {
		// mdhd timescale equals the audio sample rate for raw audio.
		if st.audio.SampleRate == 0 {
			st.audio.SampleRate = mdhd.Timescale
		}
	}
	return nil, nil
}

func (st *parseState) readHdlr(h *mp4.ReadHandle) (interface{}, error) {
	box, _, err := h.ReadPayload()
	if err != nil {
		return nil, err
	}
	hdlr := box.(*mp4.Hdlr)
	switch string(hdlr.HandlerType[:]) {
	case "vide":
		st.track.Kind = "video"
		st.video = &VideoTrackSummary{TrackID: st.pendingTrackID}
		if st.movieColor != nil {
			c := *st.movieColor
			st.video.Colors = append(st.video.Colors, c)
		}
		st.track.Video = st.video
		st.summary.Tracks = append(st.summary.Tracks, st.track)
	case "soun":
		st.track.Kind = "audio"
		st.audio = &AudioTrackLayout{Codec: "aac", TrackID: st.pendingTrackID}
		st.track.Audio = st.audio
		st.summary.Tracks = append(st.summary.Tracks, st.track)
	default:
		st.track.Kind = "other"
	}
	return nil, nil
}

func (st *parseState) readStts(h *mp4.ReadHandle) (interface{}, error) {
	box, _, err := h.ReadPayload()
	if err != nil {
		return nil, err
	}
	stts := box.(*mp4.Stts)
	var total uint64
	for _, e := range stts.Entries {
		total += uint64(e.SampleCount)
	}
	if st.video != nil {
		st.video.SampleCount = total
	}
	if st.audio != nil {
		st.audio.SampleCount = total
	}
	return nil, nil
}

func (st *parseState) readStsz(h *mp4.ReadHandle) (interface{}, error) {
	box, _, err := h.ReadPayload()
	if err != nil {
		return nil, err
	}
	stsz := box.(*mp4.Stsz)
	declared := uint64(stsz.SampleCount)
	actual := uint64(len(stsz.EntrySize))
	// A variable-size table must carry exactly SampleCount entries; a
	// mismatch means the sample table was damaged.
	if stsz.SampleSize == 0 && actual != declared {
		return nil, model.New(model.KindCorrupt, "ingest", "mp4-parse",
			"stsz sample count does not match entry sizes")
	}
	// A constant-size table still needs a positive count.
	if stsz.SampleSize != 0 && declared == 0 {
		return nil, model.New(model.KindCorrupt, "ingest", "mp4-parse",
			"stsz declares zero samples")
	}
	return nil, nil
}

func (st *parseState) readStss(h *mp4.ReadHandle) (interface{}, error) {
	box, _, err := h.ReadPayload()
	if err != nil {
		return nil, err
	}
	stss := box.(*mp4.Stss)
	if stss.EntryCount == 0 || len(stss.SampleNumber) == 0 {
		return nil, model.New(model.KindCorrupt, "ingest", "mp4-parse",
			"missing keyframe index (empty stss)")
	}
	if st.video != nil {
		st.video.KeyframeIndexes = append(st.video.KeyframeIndexes,
			stss.SampleNumber...)
	}
	return nil, nil
}

func (st *parseState) readStsdColr(h *mp4.ReadHandle) (interface{}, error) {
	c, err := readColr(h)
	if err != nil {
		return nil, err
	}
	c.Source = "track"
	if st.video != nil {
		st.video.Colors = append(st.video.Colors, *c)
	}
	return nil, nil
}

func (st *parseState) readTrakColr(h *mp4.ReadHandle) (interface{}, error) {
	c, err := readColr(h)
	if err != nil {
		return nil, err
	}
	c.Source = "container"
	if st.video != nil {
		st.video.Colors = append(st.video.Colors, *c)
	}
	return nil, nil
}

// readMovieColr stashes the moov-level color; it is attached to every
// video track when that track is created.
func (st *parseState) readMovieColr(h *mp4.ReadHandle) (interface{}, error) {
	c, err := readColr(h)
	if err != nil {
		return nil, err
	}
	c.Source = "container"
	st.movieColor = c
	return nil, nil
}

func readColr(h *mp4.ReadHandle) (*Color, error) {
	box, _, err := h.ReadPayload()
	if err != nil {
		return nil, err
	}
	colr := box.(*mp4.Colr)
	return &Color{
		Primaries:  colr.ColourPrimaries,
		Transfer:   colr.TransferCharacteristics,
		Matrix:     colr.MatrixCoefficients,
		ICCProfile: string(colr.ColourType[:]) == "rICC",
	}, nil
}

func (st *parseState) readStsdClap(h *mp4.ReadHandle) (interface{}, error) {
	c, err := readClap(h)
	if err != nil {
		return nil, err
	}
	if st.video != nil {
		st.video.Crop = c
	}
	return nil, nil
}

func (st *parseState) readTrakClap(h *mp4.ReadHandle) (interface{}, error) {
	c, err := readClap(h)
	if err != nil {
		return nil, err
	}
	if st.track != nil {
		st.track.ContainerCrop = c
	}
	return nil, nil
}

// readClap decodes the 32-byte clean aperture payload via RawBox.
func readClap(h *mp4.ReadHandle) (*Crop, error) {
	box, _, err := h.ReadPayload()
	if err != nil {
		return nil, err
	}
	raw := box.(interface{ Payload() []byte })
	if len(raw.Payload()) < 32 {
		return nil, model.New(model.KindCorrupt, "ingest", "mp4-parse",
			"clap payload truncated")
	}
	d := raw.Payload()
	return &Crop{
		Width:  int64(int32(binary.BigEndian.Uint32(d[0:4]))),
		Height: int64(int32(binary.BigEndian.Uint32(d[8:12]))),
		X:      int64(int32(binary.BigEndian.Uint32(d[16:20]))),
		Y:      int64(int32(binary.BigEndian.Uint32(d[24:28]))),
	}, nil
}

func (st *parseState) readAvc1(h *mp4.ReadHandle) (interface{}, error) {
	if st.video == nil {
		return nil, nil
	}
	box, _, err := h.ReadPayload()
	if err != nil {
		return nil, err
	}
	d := box.(interface{ Payload() []byte }).Payload()
	if len(d) < 28 {
		return nil, model.New(model.KindCorrupt, "ingest", "mp4-parse",
			"avc1 sample entry truncated")
	}
	st.video.Codec = "h264"
	st.video.Width = uint32(binary.BigEndian.Uint16(d[24:26]))
	st.video.Height = uint32(binary.BigEndian.Uint16(d[26:28]))
	return nil, nil
}

func (st *parseState) readMpa4(h *mp4.ReadHandle) (interface{}, error) {
	if st.audio == nil {
		return nil, nil
	}
	box, _, err := h.ReadPayload()
	if err != nil {
		return nil, err
	}
	d := box.(interface{ Payload() []byte }).Payload()
	if len(d) < 28 {
		return nil, model.New(model.KindCorrupt, "ingest", "mp4-parse",
			"mp4a sample entry truncated")
	}
	st.audio.Codec = "aac"
	st.audio.Channels = binary.BigEndian.Uint16(d[16:18])
	if rate := binary.BigEndian.Uint32(d[24:28]) >> 16; rate != 0 {
		st.audio.SampleRate = rate
	}
	return nil, nil
}

func (st *parseState) readChnl(h *mp4.ReadHandle) (interface{}, error) {
	box, _, err := h.ReadPayload()
	if err != nil {
		return nil, err
	}
	raw := box.(*RawBox)
	var labels []string
	cur := []byte{}
	for _, b := range raw.Data {
		if b == 0 {
			if len(cur) > 0 {
				labels = append(labels, string(cur))
				cur = cur[:0]
			}
			continue
		}
		cur = append(cur, b)
	}
	if len(cur) > 0 {
		labels = append(labels, string(cur))
	}
	if st.audio != nil {
		st.audio.ChannelLayout = labels
	}
	return nil, nil
}

// rotationFromMatrix decodes the display matrix to degrees. The matrix
// values are fixed-point 16.16 in tkhd/mvhd.
func rotationFromMatrix(m [9]int32) Rotation {
	const one = 0x10000
	switch {
	case m[0] == one && m[4] == one:
		return 0
	case m[1] == one && m[3] == -one:
		return 90
	case m[0] == -one && m[4] == -one:
		return 180
	case m[1] == -one && m[3] == one:
		return 270
	default:
		return Rotation(45) // sentinel: non-orthogonal, rejected later
	}
}
