package pipeline

import (
	"bytes"
	"encoding/json"
	"os/exec"

	"avpipe/internal/abr"
	"avpipe/internal/media"
	"avpipe/internal/segment"
)

var ffmpegBin = func() string {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return "ffmpeg"
}()

// ffprobeStream is the subset of ffprobe JSON we consume.
type ffprobeStream struct {
	CodecType string `json:"codec_type"`
	BitRate   string `json:"bit_rate"`
	Duration  string `json:"duration"`
	StartPTS  string `json:"start_time"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
}

func probe(path string) (*ffprobeOutput, error) {
	cmd := exec.Command(ffprobePath, "-v", "error", "-print_format", "json",
		"-show_streams", path)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, media.NewError(media.ClassEncode, "pipeline", "ffprobe failed: "+err.Error(), 0, false)
	}
	var res ffprobeOutput
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		return nil, media.NewError(media.ClassEncode, "pipeline", "ffprobe parse failed: "+err.Error(), 0, false)
	}
	return &res, nil
}

func streamOf(out *ffprobeOutput, codec string) *ffprobeStream {
	for i := range out.Streams {
		if out.Streams[i].CodecType == codec {
			return &out.Streams[i]
		}
	}
	return nil
}

// probeVideoBitrate returns the encoded video stream bitrate in bps.
func probeVideoBitrate(path string) (int64, error) {
	out, err := probe(path)
	if err != nil {
		return 0, err
	}
	st := streamOf(out, "video")
	if st == nil || st.BitRate == "" || st.BitRate == "N/A" {
		return 0, media.NewError(media.ClassEncode, "pipeline", "no video bitrate", 0, false)
	}
	return parseInt64(st.BitRate)
}

// probeDurationMS returns the video segment duration in milliseconds,
// falling back to any stream that reports one.
func probeDurationMS(path string) (float64, error) {
	out, err := probe(path)
	if err != nil {
		return 0, err
	}
	if st := streamOf(out, "video"); st != nil && st.Duration != "" && st.Duration != "N/A" {
		if v, err := parseFloat(st.Duration); err == nil {
			return v * 1000, nil
		}
	}
	for _, st := range out.Streams {
		if st.Duration != "" && st.Duration != "N/A" {
			v, err := parseFloat(st.Duration)
			if err == nil {
				return v * 1000, nil
			}
		}
	}
	return 0, media.NewError(media.ClassEncode, "pipeline", "duration unavailable", 0, false)
}

// verifySegment probes one encoded segment and computes its luma PSNR
// against the downscaled source frames and its AV drift on the shared clock.
func (r *Runner) verifySegment(g uint32, tier segment.Tier, path string,
	frames []*media.Frame, tl *Timeline) (SegmentReport, error) {
	row := SegmentReport{Group: int(g), Tier: tier, TargetKbps: tier.Kbps, Path: path}
	bps, err := probeVideoBitrate(path)
	if err != nil {
		return row, err
	}
	row.ActualBps = bps
	row.DeltaPct = abr.BitrateDeltaPct(tier.Kbps, bps)
	row.BitrateOK = abr.WithinTolerance(tier.Kbps, bps)

	ms, err := probeDurationMS(path)
	if err == nil {
		row.DurationMS = ms
	}
	// Both streams start at the same segment origin on the 90 kHz clock.
	row.DriftMS = abr.AVDriftMS(media.VideoPTS(g*uint32(media.VideoFPS)),
		media.AudioPTS(int(g)*media.AudioRate))

	psnr, err := decodeAndScore(path, tier, frames)
	if err != nil {
		return row, err
	}
	row.PSNR = psnr
	return row, nil
}

// decodeAndScore decodes only the video stream of the segment to raw I420
// at the tier resolution and computes mean luma PSNR against bilinear-
// downscaled source frames. Decoding video alone avoids extra frames that
// the longer audio track would otherwise emit.
func decodeAndScore(path string, tier segment.Tier, frames []*media.Frame) (float64, error) {
	cmd := exec.Command(ffmpegBin, "-hide_banner", "-loglevel", "error",
		"-i", path, "-map", "0:v:0", "-fps_mode", "passthrough",
		"-f", "rawvideo", "-pix_fmt", "yuv420p", "-")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return 0, media.NewError(media.ClassEncode, "pipeline", "decode for PSNR failed: "+err.Error(), 0, false)
	}
	planeY := tier.Width * tier.Height
	planeC := (tier.Width / 2) * (tier.Height / 2)
	frameSize := planeY + 2*planeC
	raw := out.Bytes()
	available := len(raw) / frameSize
	n := len(frames)
	if available < n {
		n = available
	}
	if n == 0 {
		return 0, media.NewError(media.ClassEncode, "pipeline", "no decoded frames for PSNR", 0, false)
	}
	var values []float64
	for i := 0; i < n; i++ {
		got := &media.Frame{Width: tier.Width, Height: tier.Height,
			Y: raw[i*frameSize : i*frameSize+planeY]}
		ref := abr.DownscaleI420(frames[i], tier.Width, tier.Height)
		values = append(values, abr.FramePSNR(ref, got))
	}
	return abr.MeanPSNR(values), nil
}
