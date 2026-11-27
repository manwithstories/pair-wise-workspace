// Package probe inspects local media segments with go-astiav (FFmpeg) and
// returns format-agnostic stream specifications. Input files are opened
// read-only; nothing here writes to the source tree.
package probe

import (
	"fmt"

	"github.com/asticode/go-astiav"
)

// ColorInfo captures the colour metadata of one video stream.
type ColorInfo struct {
	Primaries  string `json:"primaries"`
	Transfer   string `json:"transfer"`
	Space      string `json:"space"`
	Range      string `json:"range"`
	// Explicit is false when the source left colour metadata unspecified.
	Explicit bool `json:"explicit"`
}

// StreamInfo is one probed track.
type StreamInfo struct {
	Index      int    `json:"index"`
	MediaType  string `json:"media_type"`
	CodecName  string `json:"codec_name"`
	CodecID    int    `json:"codec_id"`
	TimeBaseNum int   `json:"time_base_num"`
	TimeBaseDen int   `json:"time_base_den"`
	// Video fields.
	Width       int       `json:"width,omitempty"`
	Height      int       `json:"height,omitempty"`
	PixelFormat string    `json:"pixel_format,omitempty"`
	Color       ColorInfo `json:"color"`
	FrameRateNum int      `json:"frame_rate_num,omitempty"`
	FrameRateDen int      `json:"frame_rate_den,omitempty"`
	// Audio fields.
	SampleRate   int    `json:"sample_rate,omitempty"`
	Channels     int    `json:"channels,omitempty"`
	ChannelLayout string `json:"channel_layout,omitempty"`
	SampleFormat string `json:"sample_format,omitempty"`
	// BitRate is the stream-level declared bitrate when available.
	BitRate int64 `json:"bit_rate,omitempty"`
}

// Report is the result of probing one segment.
type Report struct {
	Path        string       `json:"path"`
	FormatName  string       `json:"format_name"`
	Duration90k int64        `json:"duration_90k"`
	Streams     []StreamInfo `json:"streams"`
}

// Probe opens path read-only, reads stream info and closes it. The returned
// Report contains no CGo references and is safe to retain.
func Probe(path string) (*Report, error) {
	fc := astiav.AllocFormatContext()
	if fc == nil {
		return nil, fmt.Errorf("probe: alloc format context failed for %s", path)
	}
	defer fc.Free()

	if err := fc.OpenInput(path, nil, nil); err != nil {
		return nil, fmt.Errorf("probe: open %s: %w", path, err)
	}
	defer fc.CloseInput()
	if err := fc.FindStreamInfo(nil); err != nil {
		return nil, fmt.Errorf("probe: stream info %s: %w", path, err)
	}

	rep := &Report{Path: path}
	if of := fc.OutputFormat(); of != nil {
		rep.FormatName = of.Name()
	}
	if ifmt := fc.InputFormat(); ifmt != nil {
		rep.FormatName = ifmt.Name()
	}
	if fc.Duration() != astiav.NoPtsValue {
		// Format context duration is in AV_TIME_BASE units (microseconds).
		rep.Duration90k = fc.Duration() * 90000 / 1_000_000
	}

	for _, st := range fc.Streams() {
		cp := st.CodecParameters()
		info := StreamInfo{Index: st.Index(), CodecID: int(cp.CodecID())}
		tb := st.TimeBase()
		info.TimeBaseNum, info.TimeBaseDen = tb.Num(), tb.Den()
		fr := fc.GuessFrameRate(st, nil)
		info.FrameRateNum, info.FrameRateDen = fr.Num(), fr.Den()
		info.BitRate = cp.BitRate()

		switch cp.MediaType() {
		case astiav.MediaTypeVideo:
			fillVideo(&info, cp)
		case astiav.MediaTypeAudio:
			fillAudio(&info, cp)
		default:
			info.MediaType = "other"
			continue
		}
		rep.Streams = append(rep.Streams, info)
	}
	return rep, nil
}

func fillVideo(info *StreamInfo, cp *astiav.CodecParameters) {
	info.MediaType = "video"
	info.Width = cp.Width()
	info.Height = cp.Height()
	info.PixelFormat = cp.PixelFormat().String()
	if c := astiav.FindDecoder(cp.CodecID()); c != nil {
		info.CodecName = c.Name()
	}
	col := ColorInfo{
		Primaries: primaryName(cp.ColorPrimaries()),
		Transfer:  transferName(cp.ColorTransferCharacteristic()),
		Space:     cp.ColorSpace().String(),
		Range:     cp.ColorRange().String(),
	}
	col.Explicit = cp.ColorPrimaries() != astiav.ColorPrimariesUnspecified ||
		cp.ColorSpace() != astiav.ColorSpaceUnspecified ||
		cp.ColorRange() != astiav.ColorRangeUnspecified
	info.Color = col
}

func fillAudio(info *StreamInfo, cp *astiav.CodecParameters) {
	info.MediaType = "audio"
	info.SampleRate = cp.SampleRate()
	layout := cp.ChannelLayout()
	info.Channels = layout.Channels()
	info.ChannelLayout = describeLayout(layout)
	info.SampleFormat = cp.SampleFormat().String()
	if c := astiav.FindDecoder(cp.CodecID()); c != nil {
		info.CodecName = c.Name()
	}
}

// describeLayout maps the common channel layouts used in the delivery spec to
// stable names, falling back to a numeric description.
func describeLayout(l astiav.ChannelLayout) string {
	switch {
	case l.Equal(astiav.ChannelLayoutMono):
		return "mono"
	case l.Equal(astiav.ChannelLayoutStereo):
		return "stereo"
	case l.Equal(astiav.ChannelLayout5Point1), l.Equal(astiav.ChannelLayout5Point1Back):
		return "5.1"
	default:
		return fmt.Sprintf("ch%d", l.Channels())
	}
}

func primaryName(p astiav.ColorPrimaries) string {
	switch p {
	case astiav.ColorPrimariesUnspecified:
		return "unspecified"
	case astiav.ColorPrimariesBt709:
		return "bt709"
	case astiav.ColorPrimariesBt2020:
		return "bt2020"
	case astiav.ColorPrimariesSmpte170M:
		return "smpte170m"
	case astiav.ColorPrimariesSmpte240M:
		return "smpte240m"
	case astiav.ColorPrimariesFilm:
		return "film"
	default:
		return fmt.Sprintf("pri%d", int(p))
	}
}

func transferName(t astiav.ColorTransferCharacteristic) string {
	switch t {
	case astiav.ColorTransferCharacteristicUnspecified:
		return "unspecified"
	case astiav.ColorTransferCharacteristicBt709:
		return "bt709"
	case astiav.ColorTransferCharacteristicGamma22:
		return "gamma22"
	case astiav.ColorTransferCharacteristicGamma28:
		return "gamma28"
	case astiav.ColorTransferCharacteristicSmpte2084:
		return "smpte2084"
	default:
		return fmt.Sprintf("trc%d", int(t))
	}
}
