package wav

import (
	"fmt"
	"io"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"

	"avpipe/internal/media"
)

// Recorder streams normalized blocks into a 16-bit PCM WAV file. It reuses the
// encoder session across blocks so the data chunk header is finalized once on
// Close, and it enforces that every block arrives at the negotiated format,
// contiguously, without overlap.
type Recorder struct {
	enc      *wav.Encoder
	rate     media.Rate
	channels int
	next     int64
	wrote    bool
	closed   bool
}

// NewRecorder starts a session on w.
func NewRecorder(w io.WriteSeeker, rate media.Rate, channels int) *Recorder {
	return &Recorder{
		enc:      wav.NewEncoder(w, int(rate.Float()), 16, channels, 1),
		rate:     rate,
		channels: channels,
	}
}

// Write appends one block.
func (r *Recorder) Write(b media.Block) error {
	if r.closed {
		return media.NewError(media.PhaseIO, media.ExitInternal, b.Track, b.Seq, false, "recorder",
			fmt.Errorf("write after close"))
	}
	if b.Rate != r.rate || b.Channels != r.channels {
		return media.NewError(media.PhaseIO, media.ExitInternal, b.Track, b.Seq, false, "recorder",
			fmt.Errorf("block format %s/%d does not match %s/%d",
				b.Rate.String(), b.Channels, r.rate.String(), r.channels))
	}
	if r.wrote && b.Start != r.next {
		return media.NewError(media.PhaseIO, media.ExitInternal, b.Track, b.Seq, false, "recorder",
			fmt.Errorf("block starts at %d, expected %d", b.Start, r.next))
	}
	buf := &audio.IntBuffer{
		Data:           make([]int, len(b.Samples)),
		Format:         &audio.Format{NumChannels: r.channels, SampleRate: int(r.rate.Float())},
		SourceBitDepth: 16,
	}
	for i, s := range b.Samples {
		buf.Data[i] = int(media.Quantize16(s))
	}
	if err := r.enc.Write(buf); err != nil {
		return media.NewError(media.PhaseIO, media.ExitInternal, b.Track, b.Seq, false, "recorder", err)
	}
	r.wrote = true
	r.next = b.Start + int64(b.Frames())
	return nil
}

// Close finalizes the WAV headers.
func (r *Recorder) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if err := r.enc.Close(); err != nil {
		return media.NewError(media.PhaseIO, media.ExitInternal, "", 0, false, "recorder", err)
	}
	return nil
}

// Frames reports the number of frames written.
func (r *Recorder) Frames() int64 { return r.next }
