package probe

import (
	"io"

	"github.com/abema/go-mp4"

	"mediad/internal/model"
)

func kindFromHandler(h [4]byte) model.TrackKind {
	switch string(h[:]) {
	case "vide":
		return model.Video
	case "soun":
		return model.Audio
	default:
		return model.Data
	}
}

func firstPayload(r io.ReadSeeker, parent *mp4.BoxInfo, path mp4.BoxPath) (mp4.IBox, error) {
	bs, err := payloads(r, parent, path)
	if err != nil {
		return nil, err
	}
	if len(bs) == 0 {
		return nil, nil
	}
	return bs[0], nil
}

func payloads(r io.ReadSeeker, parent *mp4.BoxInfo, path mp4.BoxPath) ([]mp4.IBox, error) {
	bs, err := mp4.ExtractBoxWithPayload(r, parent, path)
	if err != nil {
		return nil, model.Wrap(model.KindInvalidMedia, "probe.mp4", "extract "+pathName(path), err)
	}
	out := make([]mp4.IBox, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Payload)
	}
	return out, nil
}

func pathName(p mp4.BoxPath) string {
	s := ""
	for i, t := range p {
		if i > 0 {
			s += "/"
		}
		s += t.String()
	}
	return s
}

func chunkOffsets(r io.ReadSeeker, trak *mp4.BoxInfo, trackID uint32) ([]uint64, error) {
	base := mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl()}
	stcos, err := payloads(r, trak, append(append(mp4.BoxPath{}, base...), mp4.BoxTypeStco()))
	if err != nil {
		return nil, err
	}
	if len(stcos) > 0 {
		st, ok := stcos[0].(*mp4.Stco)
		if !ok {
			return nil, model.New(model.KindInvalidMedia, "probe.mp4", "stco payload type mismatch").WithTrack(trackID)
		}
		out := make([]uint64, len(st.ChunkOffset))
		for i, v := range st.ChunkOffset {
			out[i] = uint64(v)
		}
		return out, nil
	}
	co64s, err := payloads(r, trak, append(append(mp4.BoxPath{}, base...), mp4.BoxTypeCo64()))
	if err != nil {
		return nil, err
	}
	if len(co64s) > 0 {
		st, ok := co64s[0].(*mp4.Co64)
		if !ok {
			return nil, model.New(model.KindInvalidMedia, "probe.mp4", "co64 payload type mismatch").WithTrack(trackID)
		}
		return append([]uint64(nil), st.ChunkOffset...), nil
	}
	return nil, model.New(model.KindInvalidMedia, "probe.mp4", "missing stco/co64").WithTrack(trackID)
}
