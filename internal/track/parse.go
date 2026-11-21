// Package track turns raw Matroska Tracks declarations into typed source
// tracks, then builds and validates the source-to-archive track graph.
package track

import (
	"bytes"
	"io"

	"mkvarchive/internal/ebml"
	"mkvarchive/internal/model"
)

// ParseTracks walks the verbatim Tracks payload and returns every track in
// declaration order. The first malformed declaration aborts the parse and is
// returned as an offset-anchored ArchiveError.
func ParseTracks(tracksData []byte) ([]*model.Track, error) {
	r := ebml.NewReader(bytes.NewReader(tracksData))
	var out []*model.Track
	for {
		e, err := r.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if e.ID != model.IDTrackEntry {
			if err = r.Skip(e); err != nil {
				return nil, err
			}
			continue
		}
		tr, err := parseEntry(r, e)
		if err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
}

func parseEntry(parent *ebml.Reader, entry *ebml.Element) (*model.Track, error) {
	data, err := parent.ReadData(entry)
	if err != nil {
		return nil, err
	}
	r := ebml.NewReaderAtOffset(bytes.NewReader(data), entry.DataOff)
	tr := &model.Track{HeaderOffset: entry.HeaderOff, JoinUID: -1}
	for {
		e, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		body, err := r.ReadData(e)
		if err != nil {
			return nil, err
		}
		switch e.ID {
		case model.IDTrackNumber:
			tr.Number = ebml.DecodeUnsigned(body)
		case model.IDTrackUID:
			tr.UID = ebml.DecodeUnsigned(body)
		case model.IDTrackType:
			kind, ok := model.KindFromTrackType(ebml.DecodeUnsigned(body))
			if !ok {
				return nil, model.NewError("TRACK_UNSUPPORTED_TYPE",
					"track type is not video/audio/subtitle", e.HeaderOff)
			}
			tr.Kind = kind
		case model.IDCodecID:
			tr.CodecID = string(bytes.TrimRight(body, "\x00"))
		case model.IDDefaultDuration:
			tr.DefaultDuration = ebml.DecodeUnsigned(body)
		case model.IDTrackOperation:
			if uid, perr := parseTrackOperation(body, e.DataOff); perr != nil {
				return nil, perr
			} else if uid > 0 {
				tr.JoinUID = int64(uid)
			}
		case model.IDAudio:
			tr.Audio = parseAudio(body, e.DataOff)
		case model.IDVideo:
			tr.Video = parseVideo(body, e.DataOff)
		default:
			// Unknown elements are retained-or-skipped per policy; their
			// declarations never block archival of known fields.
		}
	}
	if tr.UID == 0 {
		return nil, model.NewError("TRACK_MISSING_UID",
			"TrackEntry has no TrackUID", entry.HeaderOff)
	}
	if tr.Kind == "" {
		return nil, model.NewError("TRACK_MISSING_TYPE",
			"TrackEntry has no TrackType", entry.HeaderOff)
	}
	return tr, nil
}

// parseTrackOperation walks TrackOperation/TrackJoinBlocks and returns the
// first TrackJoinUID, which is the dependency edge the track graph consumes.
func parseTrackOperation(body []byte, baseOff int64) (uint64, error) {
	op := ebml.NewReaderAtOffset(bytes.NewReader(body), baseOff)
	for {
		e, err := op.Next()
		if err == io.EOF {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		if e.ID != model.IDTrackJoinBlocks {
			if err = op.Skip(e); err != nil {
				return 0, err
			}
			continue
		}
		joinBody, err := op.ReadData(e)
		if err != nil {
			return 0, err
		}
		jr := ebml.NewReaderAtOffset(bytes.NewReader(joinBody), e.DataOff)
		for {
			je, jerr := jr.Next()
			if jerr == io.EOF {
				return 0, nil
			}
			if jerr != nil {
				return 0, jerr
			}
			jb, jerr := jr.ReadData(je)
			if jerr != nil {
				return 0, jerr
			}
			if je.ID == model.IDTrackJoinUID {
				return ebml.DecodeUnsigned(jb), nil
			}
		}
	}
}

func parseAudio(body []byte, baseOff int64) *model.AudioMeta {
	a := &model.AudioMeta{}
	r := ebml.NewReaderAtOffset(bytes.NewReader(body), baseOff)
	for {
		e, err := r.Next()
		if err != nil {
			break
		}
		data, err := r.ReadData(e)
		if err != nil {
			break
		}
		switch e.ID {
		case model.IDChannels:
			a.Channels = ebml.DecodeUnsigned(data)
		case model.IDSamplingFreq:
			f, _ := ebml.DecodeFloat(data)
			a.SamplingFrequency = f
		case model.IDBitDepth:
			a.BitDepth = ebml.DecodeUnsigned(data)
		case model.IDChannelPositions:
			a.ChannelPositions = append([]byte(nil), data...)
			a.HasChannelPositions = true
			a.PositionsDeclaredRaw = encodeHex(data)
		}
	}
	return a
}

func parseVideo(body []byte, baseOff int64) *model.VideoMeta {
	v := &model.VideoMeta{}
	r := ebml.NewReaderAtOffset(bytes.NewReader(body), baseOff)
	for {
		e, err := r.Next()
		if err != nil {
			break
		}
		data, err := r.ReadData(e)
		if err != nil {
			break
		}
		switch e.ID {
		case model.IDPixelWidth:
			v.PixelWidth = ebml.DecodeUnsigned(data)
		case model.IDPixelHeight:
			v.PixelHeight = ebml.DecodeUnsigned(data)
		case model.IDColour:
			v.ColorPresent = true
			parseColour(v, data, e.DataOff)
		}
	}
	return v
}

func parseColour(v *model.VideoMeta, body []byte, baseOff int64) {
	r := ebml.NewReaderAtOffset(bytes.NewReader(body), baseOff)
	present := map[uint32]bool{}
	for {
		e, err := r.Next()
		if err != nil {
			break
		}
		data, err := r.ReadData(e)
		if err != nil {
			break
		}
		switch e.ID {
		case model.IDColorMatrix:
			v.MatrixCoefficients = ebml.DecodeUnsigned(data)
			present[model.IDColorMatrix] = true
		case model.IDColorRange:
			v.ColorRange = ebml.DecodeUnsigned(data)
			present[model.IDColorRange] = true
		case model.IDColorPrimaries:
			v.ColorPrimaries = ebml.DecodeUnsigned(data)
			present[model.IDColorPrimaries] = true
		case model.IDTransferChar:
			v.TransferCharacteristics = ebml.DecodeUnsigned(data)
			present[model.IDTransferChar] = true
		}
	}
	for _, id := range []uint32{model.IDColorMatrix, model.IDColorRange,
		model.IDColorPrimaries, model.IDTransferChar} {
		if !present[id] {
			v.MissingColorFields = append(v.MissingColorFields, colorFieldName(id))
		}
	}
}

func colorFieldName(id uint32) string {
	switch id {
	case model.IDColorMatrix:
		return "MatrixCoefficients"
	case model.IDColorRange:
		return "Range"
	case model.IDColorPrimaries:
		return "Primaries"
	case model.IDTransferChar:
		return "TransferCharacteristics"
	}
	return "unknown"
}

func encodeHex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, x := range b {
		out[2*i] = digits[x>>4]
		out[2*i+1] = digits[x&0x0F]
	}
	return string(out)
}
