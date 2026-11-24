package demux

import (
	"bytes"
	"io"

	"github.com/abema/go-mp4"

	"relay/internal/media"
)

// rmtaBoxType is the private relay metadata box carried inside each traf.
func rmtaBoxType() mp4.BoxType { return mp4.StrToBoxType("rmta") }

func bytesNewReader(b []byte) io.ReadSeeker { return bytes.NewReader(b) }

// readMoovTracks walks the init segment and returns one MP4TrackInfo per trak.
func readMoovTracks(r io.ReadSeeker, moov *mp4.BoxInfo) ([]MP4TrackInfo, error) {
	if _, err := moov.SeekToPayload(r); err != nil {
		return nil, err
	}
	var infos []MP4TrackInfo
	for {
		bi, err := mp4.ReadBoxInfo(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, media.NewError(media.ErrTruncated, 0, "moov child: "+err.Error())
		}
		if bi.Type == mp4.BoxTypeTrak() {
			ti, err := readTrak(r, bi)
			if err != nil {
				return nil, err
			}
			infos = append(infos, ti)
		}
		if err := skipBox(r, bi); err != nil {
			return nil, err
		}
		if atEnd(r, moov) {
			break
		}
	}
	return infos, nil
}

func readTrak(r io.ReadSeeker, trak *mp4.BoxInfo) (MP4TrackInfo, error) {
	ti := MP4TrackInfo{Timescale: 1_000_000}
	if _, err := trak.SeekToPayload(r); err != nil {
		return ti, err
	}
	for {
		bi, err := mp4.ReadBoxInfo(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return ti, media.NewError(media.ErrTruncated, 0, "trak child: "+err.Error())
		}
		payload, perr := readBoxPayload(r, bi)
		if perr == nil {
			switch bi.Type {
			case mp4.BoxTypeTkhd():
				box, _, err := mp4.UnmarshalAny(bytesReader(payload), bi.Type, uint64(len(payload)), mp4.Context{})
				if err != nil {
					return ti, media.NewError(media.ErrMetadata, 0, "tkhd: "+err.Error())
				}
				tkhd := box.(*mp4.Tkhd)
				ti.SourceID = tkhd.TrackID
				ti.Width = tkhd.Width >> 16
				ti.Height = tkhd.Height >> 16
			case mp4.BoxTypeMdhd():
				box, _, err := mp4.UnmarshalAny(bytesReader(payload), bi.Type, uint64(len(payload)), mp4.Context{})
				if err != nil {
					return ti, media.NewError(media.ErrMetadata, 0, "mdhd: "+err.Error())
				}
				ti.Timescale = box.(*mp4.Mdhd).Timescale
			case mp4.BoxTypeHdlr():
				box, _, err := mp4.UnmarshalAny(bytesReader(payload), bi.Type, uint64(len(payload)), mp4.Context{})
				if err != nil {
					return ti, media.NewError(media.ErrMetadata, 0, "hdlr: "+err.Error())
				}
				switch string(box.(*mp4.Hdlr).HandlerType[:]) {
				case "vide":
					ti.Kind = media.KindVideo
				case "soun":
					ti.Kind = media.KindAudio
				default:
					ti.Kind = media.KindUnknown
				}
			}
		}
		if err := skipBox(r, bi); err != nil {
			return ti, err
		}
		if atEnd(r, trak) {
			break
		}
	}
	if ti.Kind != media.KindVideo && ti.Kind != media.KindAudio {
		return ti, media.NewError(media.ErrMetadata, ti.SourceID, "trak missing valid hdlr")
	}
	return ti, nil
}
