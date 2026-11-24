package demux

import (
	"io"

	"github.com/abema/go-mp4"

	"relay/internal/media"
)

// readMoovTracks uses the library's full-box traversal to describe every
// trak in the init segment, avoiding hand-rolled nested box walking.
func readMoovTracks(r io.ReadSeeker, moov *mp4.BoxInfo) ([]MP4TrackInfo, error) {
	var infos []MP4TrackInfo
	handler := func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case mp4.BoxTypeTrak():
			// Register the track before descending so tkhd/mdhd/hdlr have a
			// slot to fill regardless of their order inside the trak.
			infos = append(infos, MP4TrackInfo{Timescale: 1_000_000})
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
			return nil, nil
		case mp4.BoxTypeTkhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, media.NewError(media.ErrMetadata, 0, "tkhd: "+err.Error())
			}
			tkhd := box.(*mp4.Tkhd)
			ti := &infos[len(infos)-1]
			ti.SourceID = tkhd.TrackID
			ti.Width = tkhd.Width >> 16
			ti.Height = tkhd.Height >> 16
		case mp4.BoxTypeMdhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, media.NewError(media.ErrMetadata, 0, "mdhd: "+err.Error())
			}
			infos[len(infos)-1].Timescale = box.(*mp4.Mdhd).Timescale
		case mp4.BoxTypeHdlr():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, media.NewError(media.ErrMetadata, 0, "hdlr: "+err.Error())
			}
			switch string(box.(*mp4.Hdlr).HandlerType[:]) {
			case "vide":
				infos[len(infos)-1].Kind = media.KindVideo
			case "soun":
				infos[len(infos)-1].Kind = media.KindAudio
			default:
				return nil, media.NewError(media.ErrMetadata, infos[len(infos)-1].SourceID,
					"unsupported hdlr")
			}
		default:
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	if _, err := mp4.ReadBoxStructureFromInternal(r, moov, handler); err != nil {
		return nil, err
	}
	for _, ti := range infos {
		if ti.Kind != media.KindVideo && ti.Kind != media.KindAudio {
			return nil, media.NewError(media.ErrMetadata, ti.SourceID, "trak missing valid hdlr")
		}
	}
	return infos, nil
}
