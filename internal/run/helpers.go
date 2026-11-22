package run

import (
	"mediaverify/internal/audio"
	"mediaverify/internal/clock"
	"mediaverify/internal/fault"
	"mediaverify/internal/fixture"
	"mediaverify/internal/manifest"
	"mediaverify/internal/packager"
	"mediaverify/internal/scan"
	"mediaverify/internal/transport"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// manifestErrorMs 计算清单声明边界与实际媒体时间线的最大偏差（毫秒）。
func manifestErrorMs(declared []clock.Ticks, actual map[int]clock.Ticks, segDur clock.Ticks) float64 {
	var maxErr float64
	for i, d := range declared {
		a, ok := actual[i]
		if !ok {
			continue
		}
		ms := float64(a-d) / float64(clock.Second) * 1000
		if ms < 0 {
			ms = -ms
		}
		if ms > maxErr {
			maxErr = ms
		}
	}
	return maxErr
}

func parseManifest(path string) (*manifest.Program, []clock.Ticks, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fault.New(fault.KindManifest, fault.Fail, path, 0, -1, "read: %v", err)
	}
	var prog *manifest.Program
	if strings.HasSuffix(path, ".mpd") {
		prog, err = manifest.ParseDASH(path, strings.NewReader(string(b)))
	} else {
		prog, err = manifest.ParseHLS(path, strings.NewReader(string(b)))
	}
	if err != nil {
		return nil, nil, err
	}
	var declared []clock.Ticks
	var acc clock.Ticks
	for _, s := range prog.Representations[0].Segments {
		declared = append(declared, acc)
		acc += s.Duration
	}
	return prog, declared, nil
}

func probeSource(dir string, idx *scan.Index) (rate, channels, framesPerSeg int, err error) {
	if len(idx.Segments) == 0 {
		return 0, 0, 0, fault.New(fault.KindIndex, fault.Rescan, idx.ManifestPath, 0, -1, "empty index")
	}
	b, err := os.ReadFile(filepath.Join(dir, idx.Segments[0].URI))
	if err != nil {
		return 0, 0, 0, fault.New(fault.KindCorrupt, fault.Fail, idx.Segments[0].URI, 0, 0, "%v", err)
	}
	rate, channels, framesPerSeg, _, err = fixture.ReadPCMSegment(b)
	if err != nil {
		return 0, 0, 0, fault.New(fault.KindCorrupt, fault.Fail, idx.Segments[0].URI, 0, 0, "%v", err)
	}
	return rate, channels, framesPerSeg, nil
}

// meterInputs 流式预扫描输入响度（分块读取，不缓存整节目）。
func meterInputs(dir string, idx *scan.Index, meter *audio.Meter) error {
	for _, seg := range idx.Segments {
		b, err := os.ReadFile(filepath.Join(dir, seg.URI))
		if err != nil {
			return fault.New(fault.KindCorrupt, fault.Fail, seg.URI, 0, seg.Index, "%v", err)
		}
		_, _, _, payload, err := fixture.ReadPCMSegment(b)
		if err != nil {
			return fault.New(fault.KindCorrupt, fault.Fail, seg.URI, 0, seg.Index, "%v", err)
		}
		meter.Add(s16ToFloat(payload))
	}
	return nil
}

func s16ToFloat(b []byte) []float64 {
	out := make([]float64, len(b)/2)
	for i := range out {
		out[i] = float64(int16(uint16(b[2*i])|uint16(b[2*i+1])<<8)) / 32768
	}
	return out
}

func unitSamples(u transport.Unit, srcCh, frameSamples int) ([]float64, error) {
	if u.Silence {
		return make([]float64, frameSamples*srcCh), nil
	}
	if len(u.Packet.Payload)%(2*srcCh) != 0 {
		return nil, fault.New(fault.KindAudio, fault.Fail, "rtp", 0, -1,
			"non-integral frame: %d payload bytes", len(u.Packet.Payload))
	}
	return s16ToFloat(u.Packet.Payload), nil
}

func saveCK(p Params, idx *scan.Index, doneSegs int, out *os.File, frames int64) error {
	if p.CheckpointPath == "" {
		return nil
	}
	off, err := out.Seek(0, 1)
	if err != nil {
		return err
	}
	return SaveCheckpoint(p.CheckpointPath, &Checkpoint{
		InputDigest: idx.InputDigest, CompletedSegments: doneSegs,
		OutputBytes: off, Frames: frames,
	})
}

func rescan(p Params) (*scan.Index, error) {
	prog, _, err := parseManifest(p.ManifestPath)
	if err != nil {
		return nil, err
	}
	idx, err := scan.Build(prog, p.Dir, p.ManifestPath)
	if err != nil {
		return nil, err
	}
	return idx, idx.Save(p.IndexPath)
}

func openPackager(p Params, srcRate, resumedFrom int, resumeFrames int64) (*packager.Packager, *os.File, error) {
	frameSamples := p.OutRate / 50
	frameBytes := frameBytesFor(p.OutRate, p.OutChannels)
	if resumedFrom > 0 {
		out, err := os.OpenFile(p.OutputPath, os.O_RDWR, 0o644)
		if err != nil {
			return nil, nil, err
		}
		pkg, err := packager.Resume(out, p.OutRate, p.OutChannels, frameSamples, frameBytes, p.TargetKbps, resumeFrames)
		if err != nil {
			out.Close()
			return nil, nil, err
		}
		return pkg, out, nil
	}
	out, err := os.Create(p.OutputPath)
	if err != nil {
		return nil, nil, err
	}
	pkg, err := packager.New(out, p.OutRate, p.OutChannels, frameSamples, frameBytes, p.TargetKbps)
	if err != nil {
		out.Close()
		return nil, nil, err
	}
	return pkg, out, nil
}

// frameBytesFor 每帧打包字节数：2-bit 量化，4 采样/字节。
func frameBytesFor(rate, channels int) int {
	return rate / 50 * channels / 4
}

// quantize2Bit 把 float 采样确定性量化为 2-bit 并打包（4 采样/字节）。
// 这是交付用的固定格式：48kHz 立体声下媒体码率 192kbps，
// 再由封装器 CBR 填充至目标码率。
func quantize2Bit(f []float64) []byte {
	out := make([]byte, (len(f)+3)/4)
	for i, v := range f {
		lvl := int((v + 1) * 2)
		if lvl < 0 {
			lvl = 0
		}
		if lvl > 3 {
			lvl = 3
		}
		out[i/4] |= byte(lvl) << (6 - 2*(i%4))
	}
	return out
}

// pacer 带宽限速：只影响读取节奏，不改变媒体时间线与输出顺序。
type pacer struct {
	bwKbps int
	sleep  func(time.Duration)
	n      int
}

func (pc *pacer) add(bytes, dropAt, dropKbps int) {
	bw := pc.bwKbps
	if dropAt > 0 && pc.n >= dropAt && dropKbps > 0 {
		bw = dropKbps
	}
	pc.n++
	if bw <= 0 {
		return
	}
	pc.sleep(time.Duration(int64(bytes)*8*int64(time.Second)) / time.Duration(int64(bw)*1000))
}
