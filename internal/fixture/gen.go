// Package fixture 生成演示与测试用样例数据：
// 三机位、三档码率 DASH、含两处 discontinuity 的 HLS 及故障夹具。
package fixture

import (
	"fmt"
	"os"
	"path/filepath"

	"review/internal/fragment"
	"review/internal/render"
)

// 样例参数。
const (
	VideoTimescale  = 90000
	AudioTimescale  = 48000
	FramesPerCamera = 40
	FramesPerSeg    = 10
	SegmentsPerRep  = FramesPerCamera / FramesPerSeg
	FrameW, FrameH  = 160, 90
	VideoFrameDur   = 3000 // 90000/30
	AudioBlockDur   = 1600 // 48000/30
	AudioRate       = 48000
	AudioChannels   = 2
)

// Camera 一台机位的产物布局。
type Camera struct {
	ID           string
	ClockOffset  int64 // 90000 时基下的时钟偏移 tick（0/720/1350 ≈ 0/8/15ms）
	MPDPath      string
	VideoInit    [3]string // 三档码率 init
	VideoSegs    [3][]string
	AudioInit    string
	AudioSegs    []string
	RepBandwidth [3]int64
}

// HLSLayout HLS 节目轨布局。
type HLSLayout struct {
	M3U8Path string
	InitPath string
	SegPaths []string
}

// Layout 全部样例数据布局。
type Layout struct {
	Dir         string
	Cameras     []Camera
	HLS         HLSLayout
	CorruptPath string // CRC 损坏夹具
}

// CameraIDs 三机位。
var CameraIDs = []string{"camA", "camB", "camC"}

// RepIDs 三档码率。
var RepIDs = []string{"hi", "mid", "lo"}

var repQuality = [3]int{85, 60, 40}
var repBandwidth = [3]int64{800000, 400000, 200000}

// FrameSeed 帧图样种子（确定性）。
func FrameSeed(cam, frame int) int { return cam*10000 + frame }

// VideoPayload 生成一帧 JPEG 负载。
func VideoPayload(cam, frame, quality int) ([]byte, error) {
	return render.EncodeJPEG(render.MakeFrameImage(FrameSeed(cam, frame), FrameW, FrameH), quality)
}

// AudioPayload 生成一块确定性 PCM s16le 负载。
func AudioPayload(cam, block int) []byte {
	buf := make([]byte, AudioBlockDur*AudioChannels*2)
	for i := 0; i < AudioBlockDur*AudioChannels; i++ {
		v := int16(((i*31 + cam*7 + block*13) % 512) - 256)
		v <<= 6
		buf[2*i] = byte(v)
		buf[2*i+1] = byte(v >> 8)
	}
	return buf
}

// Generate 在 dir 下生成全部样例数据。
func Generate(dir string) (*Layout, error) {
	l := &Layout{Dir: dir}
	for c, id := range CameraIDs {
		cam := Camera{ID: id, ClockOffset: int64(c) * 720, RepBandwidth: repBandwidth}
		cdir := filepath.Join(dir, id)
		if err := os.MkdirAll(cdir, 0o755); err != nil {
			return nil, err
		}
		for r, rid := range RepIDs {
			repID := fmt.Sprintf("%s-v-%s", id, rid)
			init := &fragment.InitInfo{Codec: "jpeg", Timescale: VideoTimescale,
				Width: FrameW, Height: FrameH, FrameDuration: VideoFrameDur}
			ip := filepath.Join(cdir, "init-"+repID+".mp4")
			if err := fragment.WriteInit(ip, init); err != nil {
				return nil, err
			}
			cam.VideoInit[r] = ip
			for s := 0; s < SegmentsPerRep; s++ {
				var payloads [][]byte
				for f := 0; f < FramesPerSeg; f++ {
					p, err := VideoPayload(c, s*FramesPerSeg+f, repQuality[r])
					if err != nil {
						return nil, err
					}
					payloads = append(payloads, p)
				}
				sp := filepath.Join(cdir, fmt.Sprintf("seg-%s-%05d.m4s", repID, s+1))
				base := cam.ClockOffset + int64(s*FramesPerSeg*VideoFrameDur)
				if _, err := fragment.WriteSegment(sp, base, payloads, VideoFrameDur, 0); err != nil {
					return nil, err
				}
				cam.VideoSegs[r] = append(cam.VideoSegs[r], sp)
			}
		}
		// 音频轨
		aID := id + "-a"
		aInit := &fragment.InitInfo{Codec: "lpcm", Timescale: AudioTimescale,
			SampleRate: AudioRate, Channels: AudioChannels, FrameDuration: AudioBlockDur}
		ap := filepath.Join(cdir, "init-"+aID+".mp4")
		if err := fragment.WriteInit(ap, aInit); err != nil {
			return nil, err
		}
		cam.AudioInit = ap
		for s := 0; s < SegmentsPerRep; s++ {
			var payloads [][]byte
			for b := 0; b < FramesPerSeg; b++ {
				payloads = append(payloads, AudioPayload(c, s*FramesPerSeg+b))
			}
			sp := filepath.Join(cdir, fmt.Sprintf("seg-%s-%05d.m4s", aID, s+1))
			base := cam.ClockOffset*AudioTimescale/VideoTimescale + int64(s*FramesPerSeg*AudioBlockDur)
			if _, err := fragment.WriteSegment(sp, base, payloads, AudioBlockDur, 0); err != nil {
				return nil, err
			}
			cam.AudioSegs = append(cam.AudioSegs, sp)
		}
		cam.MPDPath = filepath.Join(cdir, "manifest.mpd")
		if err := os.WriteFile(cam.MPDPath, []byte(mpdText(id)), 0o644); err != nil {
			return nil, err
		}
		l.Cameras = append(l.Cameras, cam)
	}
	if err := generateHLS(dir, l); err != nil {
		return nil, err
	}
	if err := generateCorrupt(l); err != nil {
		return nil, err
	}
	return l, nil
}
