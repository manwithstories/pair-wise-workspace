package source

import (
	"errors"
	"os"

	astiav "github.com/asticode/go-astiav"

	"mediapipe/internal/media"
)

type StreamInfo struct {
	Index      int       `json:"index"`
	Kind       string    `json:"kind"`
	Codec      string    `json:"codec"`
	TimeBase   media.Rat `json:"time_base"`
	StartTime  int64     `json:"start_time"`
	Duration   int64     `json:"duration"`
	NbFrames   int64     `json:"nb_frames"`
	Width      int       `json:"width,omitempty"`
	Height     int       `json:"height,omitempty"`
	SampleRate int       `json:"sample_rate,omitempty"`
	Channels   int       `json:"channels,omitempty"`
}

type Info struct {
	Path        string       `json:"path"`
	Format      string       `json:"format"`
	DurationSec float64      `json:"duration_sec"`
	BitRate     int64        `json:"bit_rate"`
	Boxes       *BoxReport   `json:"boxes,omitempty"`
	Streams     []StreamInfo `json:"streams"`
}

type DemuxResult struct {
	Info         *Info          `json:"info"`
	Packets      []media.Packet `json:"-"`
	DecodeErrors int            `json:"decode_errors"`
}

// Probe opens MP4/MOV through go-astiav and combines FFmpeg stream metadata
// with the explicit ISO-BMFF/edit-list parse.
func Probe(path string) (*Info, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, media.WrapError(media.CodeIO, "stat input", err)
	}
	boxes, boxErr := InspectBoxes(path)
	if boxErr != nil {
		return nil, boxErr
	}
	if boxes == nil || !boxes.MoovPresent || !boxes.MoovComplete {
		return nil, media.NewError(media.CodeTruncatedMoov, "moov atom is missing or truncated", nil)
	}
	fc := astiav.AllocFormatContext()
	if fc == nil {
		return nil, media.NewError(media.CodeDemux, "alloc format context failed", nil)
	}
	defer fc.Free()
	if err := fc.OpenInput(path, nil, nil); err != nil {
		if boxes.Truncated || !boxes.MoovComplete {
			return nil, media.WrapError(media.CodeTruncatedMoov, "open truncated MP4/MOV", err)
		}
		return nil, media.WrapError(media.CodeDemux, "open input", err)
	}
	defer fc.CloseInput()
	if err := fc.FindStreamInfo(nil); err != nil {
		if boxes.Truncated || !boxes.MoovComplete {
			return nil, media.WrapError(media.CodeTruncatedMoov, "find stream info in truncated container", err)
		}
		return nil, media.WrapError(media.CodeDemux, "find stream info", err)
	}
	info := &Info{Path: path, Format: fc.InputFormat().Name(), DurationSec: float64(fc.Duration()) / 1_000_000,
		BitRate: fc.BitRate(), Boxes: boxes}
	for _, st := range fc.Streams() {
		si := StreamInfo{Index: st.Index(), TimeBase: fromRat(st.TimeBase()), StartTime: st.StartTime(),
			Duration: st.Duration(), NbFrames: st.NbFrames()}
		if cp := st.CodecParameters(); cp != nil {
			si.Kind = cp.MediaType().String()
			si.Codec = cp.CodecID().String()
			si.Width, si.Height = cp.Width(), cp.Height()
			si.SampleRate = cp.SampleRate()
			si.Channels = cp.ChannelLayout().Channels()
		}
		info.Streams = append(info.Streams, si)
	}
	return info, nil
}

// Demux reads every packet and copies payloads out of FFmpeg-owned memory.
func Demux(path string) (*DemuxResult, error) {
	info, err := Probe(path)
	if err != nil {
		return nil, err
	}
	fc := astiav.AllocFormatContext()
	defer fc.Free()
	if err := fc.OpenInput(path, nil, nil); err != nil {
		return nil, media.WrapError(media.CodeDemux, "reopen input", err)
	}
	defer fc.CloseInput()
	if err := fc.FindStreamInfo(nil); err != nil {
		return nil, media.WrapError(media.CodeDemux, "stream info", err)
	}
	tbs := map[int]media.Rat{}
	for _, st := range fc.Streams() {
		tbs[st.Index()] = fromRat(st.TimeBase())
	}
	pkt := astiav.AllocPacket()
	defer pkt.Free()
	res := &DemuxResult{Info: info}
	seq := uint64(1)
	for {
		err := fc.ReadFrame(pkt)
		if errors.Is(err, astiav.ErrEof) {
			break
		}
		if err != nil {
			res.DecodeErrors++
			return res, media.WrapError(media.CodeDemux, "read frame", err)
		}
		si := pkt.StreamIndex()
		sp := media.Packet{
			Sequence: seq, StreamIndex: si, PTS: pkt.Pts(), DTS: pkt.Dts(),
			TimeBase: tbs[si], FileOffset: pkt.Pos(), Size: pkt.Size(),
			Key: pkt.Flags().Has(astiav.PacketFlagKey),
		}
		if data := pkt.Data(); len(data) > 0 {
			sp.Payload = append([]byte(nil), data...)
			if sp.Size == 0 {
				sp.Size = len(sp.Payload)
			}
		}
		res.Packets = append(res.Packets, sp)
		seq++
		pkt.Unref()
	}
	if err := ValidateDTS(res.Packets); err != nil {
		return res, err
	}
	return res, nil
}

func fromRat(r astiav.Rational) media.Rat {
	return media.NewRat(int64(r.Num()), int64(r.Den()))
}
