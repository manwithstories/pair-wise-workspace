package wav

import (
	"encoding/binary"
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
//
// After each block the recorder reads the bytes it just wrote back from disk
// and compares them with the exact PCM16 values it intended to write. That is
// the frame-by-frame round trip reported by the demo: it fails if the encoder,
// the file system or the sample conversion corrupts a single frame.
type Recorder struct {
	enc      *wav.Encoder
	ws       io.WriteSeeker
	ra       io.ReaderAt
	rate     media.Rate
	channels int
	next     int64
	wrote    bool
	closed   bool

	matched   int64
	mismatch  int64
	verifyErr error
}

// NewRecorder starts a session on w. Reading back requires w to satisfy
// io.ReaderAt as well; when it does not, round-trip verification is skipped.
func NewRecorder(w io.WriteSeeker, rate media.Rate, channels int) *Recorder {
	r := &Recorder{
		enc:      wav.NewEncoder(w, int(rate.Float()), 16, channels, 1),
		ws:       w,
		rate:     rate,
		channels: channels,
	}
	if ra, ok := w.(io.ReaderAt); ok {
		r.ra = ra
	}
	return r
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
	want := make([]byte, len(b.Samples)*2)
	for i, s := range b.Samples {
		q := media.Quantize16(s)
		buf.Data[i] = int(q)
		binary.LittleEndian.PutUint16(want[i*2:], uint16(q))
	}
	if err := r.enc.Write(buf); err != nil {
		return media.NewError(media.PhaseIO, media.ExitInternal, b.Track, b.Seq, false, "recorder", err)
	}
	r.wrote = true
	r.next = b.Start + int64(b.Frames())
	r.verifyTail(want)
	return nil
}

// verifyTail reads back the last len(want) bytes of the file and compares them
// with the intended PCM16 encoding. The comparison is frame-exact because each
// frame occupies two bytes per channel.
func (r *Recorder) verifyTail(want []byte) {
	if r.ra == nil || len(want) == 0 {
		return
	}
	end, err := r.ws.Seek(0, io.SeekEnd)
	if err != nil {
		r.verifyErr = err
		return
	}
	start := end - int64(len(want))
	if start < 0 {
		r.verifyErr = fmt.Errorf("wrote fewer bytes than expected")
		return
	}
	got := make([]byte, len(want))
	if _, err := r.ra.ReadAt(got, start); err != nil {
		r.verifyErr = err
		return
	}
	for i := 0; i < len(want); i += 2 {
		if got[i] == want[i] && got[i+1] == want[i+1] {
			r.matched++
		} else {
			r.mismatch++
		}
	}
}

// RoundTrip reports the number of frame-exact matches and mismatches recorded
// while writing. The totals count sample frames (all channels of one frame are
// compared together), so a failure of any channel marks the frame.
func (r *Recorder) RoundTrip() (matched, mismatch int64) {
	// matched/mismatch are per-sample; convert to per-frame by dividing by the
	// channel count after flooring mismatches up so a single bad channel still
	// counts as a bad frame.
	return r.matched / int64(r.channels), (r.mismatch + int64(r.channels) - 1) / int64(r.channels)
}

// VerifyError reports a read-back failure, if any.
func (r *Recorder) VerifyError() error { return r.verifyErr }

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
