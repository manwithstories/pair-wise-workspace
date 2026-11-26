// Package av wraps the libav demux, resample and encode paths used by mediad.
// No external FFmpeg process is ever spawned: everything goes through
// go-astiav.
package av

import (
	"errors"

	"github.com/asticode/go-astiav"

	"mediad/internal/media"
)

// TrackInfo describes one selected audio track after stream probing.
type TrackInfo struct {
	StreamIndex int
	CodecID     astiav.CodecID
	StartTicks  int64
	Duration    int64
	TimeBase    media.Rational
	SampleRate  int
	Channels    int
	Layout      string
	HasVideo    bool
	CodecName   string
}

// Packet is one demuxed packet with its raw presentation time in the source
// stream's time base.
type Packet struct {
	StreamIndex int
	PTS         int64
	DTS         int64
	Duration    int64
	Key         bool
	Corrupt     bool
	Reason      string
	Data        []byte
}

// Demuxer owns an input path and opens it per operation.
type Demuxer struct {
	path string
}

// NewDemuxer validates the path and prepares to open it.
func NewDemuxer(path string) (*Demuxer, error) {
	if path == "" {
		return nil, media.NewError(media.ErrDemux, "empty input path")
	}
	return &Demuxer{path: path}, nil
}

// openInput opens a format context and probes streams.
func openInput(path string) (*astiav.FormatContext, error) {
	fc := astiav.AllocFormatContext()
	if fc == nil {
		return nil, media.NewError(media.ErrDemux, "cannot allocate format context")
	}
	if err := fc.OpenInput(path, nil, nil); err != nil {
		fc.Free()
		return nil, media.NewError(media.ErrDemux, "opening %s: %v", path, err)
	}
	if err := fc.FindStreamInfo(nil); err != nil {
		fc.CloseInput()
		fc.Free()
		return nil, media.NewError(media.ErrDemux, "probing %s: %v", path, err)
	}
	return fc, nil
}

// ProbeStreams reports every audio stream in stream-index order.
func (d *Demuxer) ProbeStreams() ([]TrackInfo, error) {
	fc, err := openInput(d.path)
	if err != nil {
		return nil, err
	}
	defer func() {
		fc.CloseInput()
		fc.Free()
	}()

	hasVideo := false
	for _, s := range fc.Streams() {
		if s.CodecParameters().MediaType() == astiav.MediaTypeVideo {
			hasVideo = true
		}
	}
	var out []TrackInfo
	for _, s := range fc.Streams() {
		cp := s.CodecParameters()
		if cp.MediaType() != astiav.MediaTypeAudio {
			continue
		}
		tb := s.TimeBase()
		layout := cp.ChannelLayout()
		codecName := ""
		if desc := cp.CodecID().Name(); desc != "" {
			codecName = desc
		}
		out = append(out, TrackInfo{
			StreamIndex: s.Index(),
			CodecID:     cp.CodecID(),
			StartTicks:  s.StartTime(),
			Duration:    s.Duration(),
			TimeBase:    media.Rational{Num: int64(tb.Num()), Den: int64(tb.Den())},
			SampleRate:  cp.SampleRate(),
			Channels:    layout.Channels(),
			Layout:      layout.String(),
			HasVideo:    hasVideo,
			CodecName:   codecName,
		})
	}
	if len(out) == 0 {
		return nil, media.NewError(media.ErrUnsupported, "%s has no audio stream", d.path)
	}
	return out, nil
}

// ReadAll demuxes every packet for the given stream in arrival order. Reading
// stops at EOF; a mid-stream read failure is returned together with the
// packets recovered so far so the caller can salvage the intact range.
func (d *Demuxer) ReadAll(streamIndex int) ([]Packet, error) {
	fc, err := openInput(d.path)
	if err != nil {
		return nil, err
	}
	defer func() {
		fc.CloseInput()
		fc.Free()
	}()

	target := (*astiav.Stream)(nil)
	for _, s := range fc.Streams() {
		if s.Index() == streamIndex {
			target = s
			break
		}
	}
	if target == nil {
		return nil, media.NewError(media.ErrDemux, "stream %d not found in %s", streamIndex, d.path)
	}
	tb := target.TimeBase()
	if tb.Num() == 0 || tb.Den() == 0 {
		return nil, media.NewError(media.ErrInvalidTimebase, "stream %d has zero time base", streamIndex)
	}

	pkt := astiav.AllocPacket()
	defer pkt.Free()
	var out []Packet
	var readErr error
	for {
		if err := fc.ReadFrame(pkt); err != nil {
			if !errors.Is(err, astiav.ErrEof) {
				readErr = media.NewError(media.ErrDemux, "reading %s: %v", d.path, err)
			}
			break
		}
		if pkt.StreamIndex() != streamIndex {
			pkt.Unref()
			continue
		}
		p := Packet{
			StreamIndex: pkt.StreamIndex(),
			PTS:         pkt.Pts(),
			DTS:         pkt.Dts(),
			Duration:    pkt.Duration(),
			Key:         pkt.Flags().Has(astiav.PacketFlagKey),
			Data:        append([]byte(nil), pkt.Data()...),
		}
		if pkt.Flags().Has(astiav.PacketFlagCorrupt) {
			p.Corrupt = true
			p.Reason = "container flagged packet corrupt"
		}
		out = append(out, p)
		pkt.Unref()
	}
	return out, readErr
}

// Duration returns the container duration expressed in microseconds.
func (d *Demuxer) Duration() (media.Rational, int64, error) {
	fc, err := openInput(d.path)
	if err != nil {
		return media.Rational{}, 0, err
	}
	defer func() {
		fc.CloseInput()
		fc.Free()
	}()
	if fc.InputFormat() != nil {
		_ = fc.InputFormat()
	}
	return media.Rational{Num: 1, Den: 1_000_000}, fc.Duration(), nil
}
