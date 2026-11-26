// Package segment encodes recovered frame/audio timelines into fMP4 HLS
// segments with the local FFmpeg binary and builds the multivariant
// playlist through Eyevinn's hls-m3u8 library.
package segment

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"avpipe/internal/media"
)

// SegmentDurationSeconds is the fixed one-second segment size.
const SegmentDurationSeconds = 1

// Tier maps a target bitrate to an encoding profile.
type Tier struct {
	Kbps   int
	Width  int
	Height int
}

// Tiers is the ordered (low to high) representation table.
var Tiers = []Tier{
	{Kbps: 180, Width: 320, Height: 180},
	{Kbps: 450, Width: 640, Height: 360},
	{Kbps: 900, Width: 960, Height: 540},
}

// SourceWidth/Height describe the generated master resolution.
const (
	SourceWidth  = 640
	SourceHeight = 360
)

// EncodeRequest is one representation-segment encoding job.
type EncodeRequest struct {
	OutDir   string
	Name     string // e.g. "v180k-seg4"
	Frames   []*media.Frame
	Audio    []byte // interleaved 16-bit mono PCM covering the same second
	AudioPTS uint64
	Tier     Tier
	SourceW  int
	SourceH  int
}

// EncodeResult reports what was produced and verified.
type EncodeResult struct {
	SegmentPath string
	Bytes       int64
	DurationSec float64
	ActualBps   int64
}

// ffmpegPath resolves the binary once per process.
var ffmpegPath = func() string {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return "ffmpeg"
}()

// EncodeSegment writes one fMP4 segment plus its init file. Video is raw
// I420 piped on stdin; audio is a little-endian PCM file written next to
// the output. All segments are exactly one second so variants stay aligned.
func EncodeSegment(req EncodeRequest) (*EncodeResult, error) {
	if err := os.MkdirAll(req.OutDir, 0o755); err != nil {
		return nil, media.NewError(media.ClassEncode, "segment", "cannot create out dir: "+err.Error(), 0, false)
	}
	segPath := filepath.Join(req.OutDir, req.Name+".m4s")
	audioPath := filepath.Join(req.OutDir, req.Name+"-audio.pcm")
	if err := os.WriteFile(audioPath, req.Audio, 0o600); err != nil {
		return nil, media.NewError(media.ClassEncode, "segment", "cannot stage audio: "+err.Error(), 0, false)
	}

	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "rawvideo", "-pix_fmt", "yuv420p",
		"-s", fmt.Sprintf("%dx%d", req.SourceW, req.SourceH),
		"-framerate", fmt.Sprintf("%d", media.VideoFPS), "-i", "pipe:0",
		"-f", "s16le", "-ar", fmt.Sprintf("%d", media.AudioRate),
		"-ac", "1", "-i", audioPath,
		"-vf", fmt.Sprintf("scale=%d:%d:flags=lanczos", req.Tier.Width, req.Tier.Height),
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-b:v", fmt.Sprintf("%dk", req.Tier.Kbps),
		"-minrate", fmt.Sprintf("%dk", req.Tier.Kbps),
		"-maxrate", fmt.Sprintf("%dk", req.Tier.Kbps),
		"-bufsize", fmt.Sprintf("%dk", req.Tier.Kbps*2),
		"-nal-hrd", "cbr",
		"-x264-params", "keyint=30:min-keyint=30:scenecut=0:repeat-headers=1:filler=1",
		"-c:a", "aac", "-b:a", "64k", "-ar", fmt.Sprintf("%d", media.AudioRate), "-ac", "1",
		"-f", "mp4",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof+faststart",
		"-t", fmt.Sprintf("%d", SegmentDurationSeconds),
		segPath}
	cmd := exec.Command(ffmpegPath, args...)
	cmd.Stdin = bytes.NewReader(packI420(req.Frames, req.SourceW, req.SourceH))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, media.NewError(media.ClassEncode, "segment",
			"ffmpeg failed: "+err.Error()+" "+stderr.String(), 0, false)
	}
	info, err := os.Stat(segPath)
	if err != nil {
		return nil, media.NewError(media.ClassEncode, "segment", "segment missing after encode", 0, false)
	}
	// The fragmented MP4 is self-initializing (empty_moov); HLS playlists
	// reference each segment directly without a separate EXT-X-MAP file.
	res := &EncodeResult{
		SegmentPath: segPath,
		Bytes:       info.Size(),
		DurationSec: SegmentDurationSeconds,
		ActualBps:   int64(float64(info.Size()*8) / SegmentDurationSeconds),
	}
	return res, nil
}

// packI420 concatenates frame planes into the rawvideo byte stream ffmpeg
// expects on stdin. Gapped frames are already freeze-filled by the pipeline.
func packI420(frames []*media.Frame, w, h int) []byte {
	planeY := w * h
	planeC := (w / 2) * (h / 2)
	out := make([]byte, 0, len(frames)*(planeY+2*planeC))
	for _, f := range frames {
		out = append(out, f.Y...)
		out = append(out, f.Cb...)
		out = append(out, f.Cr...)
	}
	return out
}
