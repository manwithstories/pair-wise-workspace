// Package decode drives FFmpeg as the sole AVC/AAC decoder. It extracts coded
// sample payloads into an Annex-B elementary stream, runs ffmpeg offscreen, and
// normalises frames through disintegration/imaging.
package decode

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"archive/internal/media"
)

// DecodeError reports a decoder failure with the fixture/track context the
// acceptance contract requires.
type DecodeError struct {
	Representation string
	TrackID        uint32
	Kind           media.TrackKind
	ExitStatus     int
	Stderr         string
	Err            error
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("ffmpeg failed: representation=%s track=%d kind=%s exit=%d stderr=%q (%v)",
		e.Representation, e.TrackID, e.Kind, e.ExitStatus, strings.TrimSpace(e.Stderr), e.Err)
}

func (e *DecodeError) Unwrap() error { return e.Err }

// Request describes one offscreen decode.
type Request struct {
	Representation string
	TrackID        uint32
	Kind           media.TrackKind
	// Width/Height are the expected display dimensions (0 for audio).
	Width, Height int
	// Codec is the fourcc from stsd ("avc1"/"avc3"/"mp4a").
	Codec string
	// InitBytes is the number of leading payload bytes that form the decoder
	// configuration record (e.g. avcC) taken from the first sample entry.
	InitBytes []byte
	// Samples are the coded payloads in presentation order.
	Samples []InputSample
	// WorkDir is a scratch directory for the input stream and PNG staging.
	WorkDir string
	// Timeout bounds the child process.
	Timeout time.Duration
}

// InputSample is one coded payload plus its timing.
type InputSample struct {
	Payload    []byte
	StartUS    int64
	DurationUS int64
	Keyframe   bool
}

// Result is a decoded, normalised video sequence.
type Result struct {
	Frames     []Frame
	Width      int
	Height     int
	FrameCount int
	TimelineUS int64
}

// Frame is one normalised RGBA image with its presentation time.
type Frame struct {
	Index   int
	StartUS int64
	// Pixels is RGBA, tightly packed, row-major.
	Pixels []byte
	Width  int
	Height int
}

// FFmpegPath resolves the ffmpeg binary, failing with an actionable message.
func FFmpegPath() (string, error) {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p, nil
	}
	for _, p := range []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/usr/bin/ffmpeg"} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: ffmpeg not found on PATH (run: brew install ffmpeg)", media.ErrUnrecoverable)
}

// DecodeVideo runs FFmpeg over the supplied samples and returns normalised
// frames. DecodeError always names the representation, track and exit status.
func DecodeVideo(ctx context.Context, req Request) (*Result, error) {
	ff, err := FFmpegPath()
	if err != nil {
		return nil, &DecodeError{Representation: req.Representation, TrackID: req.TrackID, Kind: req.Kind, ExitStatus: -1, Err: err}
	}
	if req.Kind != media.KindVideo {
		return nil, &DecodeError{Representation: req.Representation, TrackID: req.TrackID, Kind: req.Kind, ExitStatus: -1, Err: fmt.Errorf("video decode requested for %s track", req.Kind)}
	}
	if len(req.Samples) == 0 {
		return nil, &DecodeError{Representation: req.Representation, TrackID: req.TrackID, Kind: req.Kind, ExitStatus: -1, Err: fmt.Errorf("no samples to decode")}
	}
	if req.WorkDir == "" {
		return nil, &DecodeError{Representation: req.Representation, TrackID: req.TrackID, Kind: req.Kind, ExitStatus: -1, Err: fmt.Errorf("work dir required")}
	}
	if err := os.MkdirAll(req.WorkDir, 0o755); err != nil {
		return nil, &DecodeError{Representation: req.Representation, TrackID: req.TrackID, Kind: req.Kind, ExitStatus: -1, Err: err}
	}
	if req.Timeout <= 0 {
		req.Timeout = 120 * time.Second
	}

	streamPath := filepath.Join(req.WorkDir, fmt.Sprintf("%s-t%d.h264", req.Representation, req.TrackID))
	if err := writeAnnexB(streamPath, req); err != nil {
		return nil, &DecodeError{Representation: req.Representation, TrackID: req.TrackID, Kind: req.Kind, ExitStatus: -1, Err: err}
	}
	if err := os.MkdirAll(filepath.Join(req.WorkDir, "frames"), 0o755); err != nil {
		return nil, &DecodeError{Representation: req.Representation, TrackID: req.TrackID, Kind: req.Kind, ExitStatus: -1, Err: err}
	}

	var stdout, stderr bytes.Buffer
	childCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	cmd := exec.CommandContext(childCtx, ff,
		"-hide_banner",
		"-f", "h264",
		"-i", streamPath,
		"-vsync", "passthrough",
		"-f", "image2pipe",
		"-vcodec", "png",
		"-",
	)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Disable FFmpeg's persistent stats line and keep errors to stderr only.
	cmd.Env = append(os.Environ(), "AV_LOG_FORCE_NOCOLOR=1")
	if err := cmd.Run(); err != nil {
		exit := -1
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		}
		return nil, &DecodeError{
			Representation: req.Representation,
			TrackID:        req.TrackID,
			Kind:           req.Kind,
			ExitStatus:     exit,
			Stderr:         tail(stderr.String(), 400),
			Err:            err,
		}
	}
	frames, err := normaliseStream(bytes.NewReader(stdout.Bytes()), req.Width, req.Height, req.Samples)
	if err != nil {
		return nil, &DecodeError{Representation: req.Representation, TrackID: req.TrackID, Kind: req.Kind, ExitStatus: 0, Stderr: tail(stderr.String(), 400), Err: err}
	}
	return &Result{
		Frames:     frames,
		Width:      req.Width,
		Height:     req.Height,
		FrameCount: len(frames),
		TimelineUS: totalDuration(req.Samples),
	}, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func totalDuration(samples []InputSample) int64 {
	if len(samples) == 0 {
		return 0
	}
	return samples[len(samples)-1].StartUS + samples[len(samples)-1].DurationUS
}

// writeAnnexB writes SPS/PPS then each sample as Annex-B start-code prefixed
// NAL units. avcC length-prefixed samples are converted per NAL; existing
// Annex-B payloads pass through unchanged.
func writeAnnexB(path string, req Request) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var total int64
	for _, s := range req.Samples {
		total += int64(len(s.Payload))
	}
	if total > 256<<20 {
		return fmt.Errorf("elementary stream %d bytes exceeds memory budget", total)
	}
	// Emit SPS/PPS placed by avcC extraction (init bytes already contain them
	// for avc1/avc3 in fixtures built by this tool).
	if len(req.InitBytes) > 0 {
		for _, nal := range req.InitBytes {
			_ = nal
		}
		buf := &bytes.Buffer{}
		if err := writeSPSPPS(buf, req.InitBytes); err != nil {
			// Non-AVCC init bytes are not fatal: samples may already carry
			// in-band parameter sets (avc3).
			if !strings.HasPrefix(req.Codec, "avc3") {
				return err
			}
		}
		if _, err := f.Write(buf.Bytes()); err != nil {
			return err
		}
	}
	for _, s := range req.Samples {
		if len(s.Payload) >= 4 && s.Payload[0] == 0 && s.Payload[1] == 0 && s.Payload[2] == 0 && s.Payload[3] == 1 {
			if _, err := f.Write(s.Payload); err != nil {
				return err
			}
			continue
		}
		if err := writeLengthPrefixed(f, s.Payload); err != nil {
			return err
		}
	}
	return f.Sync()
}

// writeLengthPrefixed converts a length-prefixed AVCC access unit to Annex-B.
func writeLengthPrefixed(w interface{ Write([]byte) (int, error) }, payload []byte) error {
	startCode := []byte{0, 0, 0, 1}
	pos := 0
	for pos+4 <= len(payload) {
		n := int(binary.BigEndian.Uint32(payload[pos : pos+4]))
		if n <= 0 || pos+4+n > len(payload) {
			// Not AVCC framing: treat the whole access unit as one NAL.
			if _, err := w.Write(startCode); err != nil {
				return err
			}
			_, err := w.Write(payload)
			return err
		}
		if _, err := w.Write(startCode); err != nil {
			return err
		}
		if _, err := w.Write(payload[pos+4 : pos+4+n]); err != nil {
			return err
		}
		pos += 4 + n
	}
	if pos != len(payload) && pos != 0 {
		return fmt.Errorf("trailing %d bytes after AVCC framing", len(payload)-pos)
	}
	return nil
}

// writeSPSPPS expands an AVCDecoderConfigurationRecord into Annex-B SPS and
// PPS NALs.
func writeSPSPPS(w interface{ Write([]byte) (int, error) }, init []byte) error {
	if len(init) < 7 || init[0] != 1 {
		return fmt.Errorf("not an AVCDecoderConfigurationRecord")
	}
	spsLen := int(init[5] & 0x1F)
	pos := 6
	var spsCount int
	for i := 0; i < spsLen; i++ {
		if pos+2 > len(init) {
			return fmt.Errorf("avcC SPS length truncated")
		}
		n := int(binary.BigEndian.Uint16(init[pos : pos+2]))
		pos += 2
		if pos+n > len(init) {
			return fmt.Errorf("avcC SPS payload truncated")
		}
		writeStartCode(w)
		if _, err := w.Write(init[pos : pos+n]); err != nil {
			return err
		}
		pos += n
		spsCount++
	}
	if spsCount == 0 {
		return fmt.Errorf("avcC carries no SPS")
	}
	if pos >= len(init) {
		return fmt.Errorf("avcC PPS list missing")
	}
	ppsLen := int(init[pos])
	pos++
	for i := 0; i < ppsLen; i++ {
		if pos+2 > len(init) {
			return fmt.Errorf("avcC PPS length truncated")
		}
		n := int(binary.BigEndian.Uint16(init[pos : pos+2]))
		pos += 2
		if pos+n > len(init) {
			return fmt.Errorf("avcC PPS payload truncated")
		}
		writeStartCode(w)
		if _, err := w.Write(init[pos : pos+n]); err != nil {
			return err
		}
		pos += n
	}
	return nil
}

func writeStartCode(w interface{ Write([]byte) (int, error) }) {
	_, _ = w.Write([]byte{0, 0, 0, 1})
}
