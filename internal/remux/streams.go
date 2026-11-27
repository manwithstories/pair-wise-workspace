package remux

import (
	"fmt"

	"github.com/asticode/go-astiav"

	"mediacheck/internal/inspect"
	"mediacheck/internal/model"
)

func (w *Writer) addStreams(streams []model.StreamInfo) error {
	// Audio first keeps stream indices stable between runs.
	for _, track := range []model.Track{model.TrackAudio, model.TrackVideo} {
		var info model.StreamInfo
		found := false
		for _, s := range streams {
			if s.Track == track {
				info, found = s, true
				break
			}
		}
		if !found {
			continue
		}
		if err := inspect.ValidateStreamInfo(info); err != nil {
			return err
		}
		st := w.ofc.NewStream(nil)
		if st == nil {
			return fmt.Errorf("remux: allocating %s stream", track)
		}
		st.SetTimeBase(astiav.NewRational(1, 1000))
		cp := st.CodecParameters()
		switch track {
		case model.TrackAudio:
			cp.SetMediaType(astiav.MediaTypeAudio)
			cp.SetSampleRate(info.SampleRate)
			cp.SetChannelLayout(channelLayout(info.Channels))
			cp.SetSampleFormat(astiav.SampleFormatFlt)
			switch info.Codec {
			case model.CodecOpus:
				cp.SetCodecID(astiav.CodecIDOpus)
			case model.CodecAAC:
				cp.SetCodecID(astiav.CodecIDAac)
			default:
				return fmt.Errorf("remux: unsupported audio codec %s: %w", info.Codec, model.ErrUnsupported)
			}
		case model.TrackVideo:
			cp.SetMediaType(astiav.MediaTypeVideo)
			cp.SetCodecID(astiav.CodecIDH264)
			cp.SetWidth(info.Width)
			cp.SetHeight(info.Height)
			cp.SetSampleAspectRatio(astiav.NewRational(1, 1))
			if info.FrameRate.Valid() {
				cp.SetFrameSize(0)
			}
		}
		if len(info.Extradata) > 0 {
			if err := cp.SetExtraData(info.Extradata); err != nil {
				return fmt.Errorf("remux: setting %s extradata: %w", track, model.ErrUnsupported)
			}
		}
		w.streams[track] = st
		w.infos[track] = info
	}
	if len(w.streams) == 0 {
		return fmt.Errorf("remux: no audio or video streams")
	}
	return nil
}

func channelLayout(channels int) astiav.ChannelLayout {
	switch channels {
	case 1:
		return astiav.ChannelLayoutMono
	case 2:
		return astiav.ChannelLayoutStereo
	default:
		return astiav.ChannelLayoutStereo
	}
}
