package run

import (
	"errors"
	"os"
	"strings"
	"time"

	"github.com/pion/rtp"

	"mediaverify/internal/audio"
	"mediaverify/internal/clock"
	"mediaverify/internal/fault"
	"mediaverify/internal/fixture"
	"mediaverify/internal/report"
	"mediaverify/internal/scan"
	"mediaverify/internal/transport"
)

// ErrInterrupted 表示按 StopAfterSegments 模拟的中断（已落检查点）。
var ErrInterrupted = errors.New("run interrupted at segment boundary (checkpoint saved)")

// Params 流式执行参数。
type Params struct {
	Dir, ManifestPath, IndexPath, OutputPath, CheckpointPath string
	TargetKbps, OutRate, OutChannels                         int
	TargetLUFS                                               float64
	LossStop                                                 bool // 突发丢失即停（否则填静音）
	BandwidthKbps, BandwidthDropAt, BandwidthDropKbps        int  // 带宽及骤降点（仅影响读取节奏）
	Packets                                                  []fixture.Packet
	Sleep                                                    func(time.Duration) // nil=真实睡眠
	StopAfterSegments                                        int                 // >0 模拟中断
	Resume                                                   bool
}

// Result 执行结果。
type Result struct {
	Metrics report.Metrics
}

// Execute 运行两阶段流程的流式执行阶段。
func Execute(p Params) (*Result, error) {
	sleep := p.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	res := &Result{}
	m := &res.Metrics
	m.TargetKbps = p.TargetKbps
	m.RecoveryAction = "none"
	m.ResumedFromSeg = -1

	// 1. 加载索引并校验输入摘要
	idx, err := scan.Load(p.IndexPath)
	if err != nil {
		return res, err
	}
	if err := idx.Verify(p.Dir); err != nil {
		return res, err
	}
	m.IndexDigestMatch = true

	// 2. 解析清单，取声明的分段边界（本地时间轴）
	prog, declared, err := parseManifest(p.ManifestPath)
	if err != nil {
		return res, err
	}
	_ = prog

	// 3. 读取源参数（首分段头）并做输入响度预扫描
	srcRate, srcCh, framesPerSeg, err := probeSource(p.Dir, idx)
	if err != nil {
		return res, err
	}
	frameSamples := srcRate / 50
	var inMeter audio.Meter
	if err := meterInputs(p.Dir, idx, &inMeter); err != nil {
		return res, err
	}
	inStats := inMeter.Stats()
	m.InputLUFS = inStats.LUFS
	gain := audio.GainToTarget(inStats.LUFS, p.TargetLUFS)
	if err := audio.CheckPeak(inStats, gain); err != nil {
		if ge, ok := err.(*audio.GainError); ok {
			m.OutputLUFS = ge.Stats.LUFS
			m.PeakDBFS = ge.Stats.PeakDBFS
		}
		return res, err
	}

	// 4. 续跑：校验检查点，不可信则丢弃并重新扫描
	resumedFrom := 0
	var resumeFrames int64
	if p.Resume && p.CheckpointPath != "" {
		ck, ckErr := LoadCheckpoint(p.CheckpointPath, idx.InputDigest, p.OutputPath)
		if ckErr == nil {
			resumedFrom = ck.CompletedSegments
			resumeFrames = ck.Frames
			m.RecoveryAction = "resumed"
			m.ResumedFromSeg = ck.CompletedSegments
		} else if fe, ok := fault.As(ckErr); ok && fe.Recovery == fault.Rescan &&
			!os.IsNotExist(ckErr) && !strings.Contains(ckErr.Error(), "no checkpoint") {
			m.RecoveryAction = "rescan"
			m.ResumedFromSeg = -1
			if _, err := rescan(p); err != nil {
				return res, err
			}
		}
	}

	// 5. 打开输出与封装器
	pkg, out, err := openPackager(p, srcRate, resumedFrom, resumeFrames)
	if err != nil {
		return res, err
	}
	defer out.Close()

	// 6. 流式主循环：RTP 恢复 → 声道归一 → 重采样 → 增益 → 封装
	policy := transport.PolicyFillSilence
	if p.LossStop {
		policy = transport.PolicyStop
	}
	reo := transport.New(16, srcRate, frameSamples, policy)
	rs, err := audio.NewResampler(srcRate, p.OutRate, p.OutChannels)
	if err != nil {
		return res, err
	}
	segDur := clock.SamplesToTicks(int64(framesPerSeg*frameSamples), srcRate)
	var outMeter audio.Meter
	var pending []float64
	framesWritten := resumeFrames
	curSeg := resumedFrom
	actualStart := map[int]clock.Ticks{}
	pace := &pacer{bwKbps: p.BandwidthKbps, sleep: sleep}

	handleUnits := func(units []transport.Unit) error {
		for _, u := range units {
			payloadLen := 0
			if !u.Silence {
				payloadLen = len(u.Packet.Payload)
			}
			pace.add(payloadLen+12, p.BandwidthDropAt, p.BandwidthDropKbps)
			segIdx := int(u.Start / segDur)
			if segIdx < resumedFrom {
				continue
			}
			if _, ok := actualStart[segIdx]; !ok {
				actualStart[segIdx] = u.Start
			}
			for segIdx > curSeg {
				curSeg++
				if err := saveCK(p, idx, curSeg, out, framesWritten); err != nil {
					return err
				}
				if p.StopAfterSegments > 0 && curSeg >= p.StopAfterSegments {
					return ErrInterrupted
				}
			}
			samples, err := unitSamples(u, srcCh, frameSamples)
			if err != nil {
				return err
			}
			samples, err = audio.NormalizeChannels(samples, srcCh, p.OutChannels)
			if err != nil {
				return err
			}
			block, err := rs.Process(samples)
			if err != nil {
				return err
			}
			audio.ApplyGain(block, gain)
			outMeter.Add(block)
			pending = append(pending, block...)
			perFrame := p.OutRate / 50 * p.OutChannels
			for len(pending) >= perFrame {
				if err := pkg.WriteFrame(quantize2Bit(pending[:perFrame])); err != nil {
					return err
				}
				pending = pending[perFrame:]
				framesWritten++
			}
		}
		return nil
	}

	for _, fp := range p.Packets {
		units, err := reo.Feed(&rtp.Packet{
			Header:  rtp.Header{SequenceNumber: fp.Seq, Timestamp: fp.TS},
			Payload: fp.Payload,
		})
		if err != nil {
			return res, err
		}
		if err := handleUnits(units); err != nil {
			return res, err
		}
	}
	units, err := reo.Flush()
	if err != nil {
		return res, err
	}
	if err := handleUnits(units); err != nil && !errors.Is(err, ErrInterrupted) {
		return res, err
	}
	// 刷新重采样器尾部并补齐末帧
	tail, err := rs.Process(make([]float64, srcCh*frameSamples))
	if err != nil {
		return res, err
	}
	audio.ApplyGain(tail, gain)
	outMeter.Add(tail)
	pending = append(pending, tail...)
	perFrame := p.OutRate / 50 * p.OutChannels
	if len(pending) > 0 && len(pending) < perFrame {
		pending = append(pending, make([]float64, perFrame-len(pending))...)
	}
	for len(pending) >= perFrame {
		if err := pkg.WriteFrame(quantize2Bit(pending[:perFrame])); err != nil {
			return res, err
		}
		pending = pending[perFrame:]
		framesWritten++
	}

	// 7. 收尾：最终检查点、封装统计、指标
	totalSegs := len(idx.Segments)
	if err := saveCK(p, idx, totalSegs, out, framesWritten); err != nil {
		return res, err
	}
	pkres, err := pkg.Close()
	if err != nil {
		return res, err
	}
	outStats := outMeter.Stats()
	m.OutputLUFS = outStats.LUFS
	m.PeakDBFS = outStats.PeakDBFS
	m.SilenceRatio = outStats.SilenceRatio
	m.ActualKbps = pkres.ActualKbps
	if p.TargetKbps > 0 {
		m.BitrateErrorPct = (pkres.ActualKbps - float64(p.TargetKbps)) / float64(p.TargetKbps) * 100
	}
	m.GapPackets = reo.Stats.GapPackets
	m.DuplicatePackets = reo.Stats.DuplicatePackets
	m.ReorderedPackets = reo.Stats.ReorderedPackets
	m.ManifestErrorMs = manifestErrorMs(declared, actualStart, segDur)
	return res, nil
}
