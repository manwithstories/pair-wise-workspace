package job

import (
	"fmt"
	"sync"

	"rcx/internal/codec"
	"rcx/internal/fixture"
	"rcx/internal/rc"
)

// Scheduler 并发调度分段编码，依赖状态机与检查点日志。
type Scheduler struct {
	Workers       int
	JournalPath   string
	MaxSegRetry   int // 段失败重试次数
	Machine       *Machine
	Controller    *rc.Controller
	Plan          *rc.Plan
	Frames        []*fixture.Frame
	OnSegmentDone func(seg SegCheckpoint) // 可选回调
}

// Run 执行全部分段，支持断点续跑：已完成且校验和一致的段直接跳过。
// 返回最终检查点；任一段不可达或失败时返回错误（检查点已保留）。
func (s *Scheduler) Run() (*Journal, error) {
	j, _, err := Load(s.JournalPath)
	if err != nil {
		return nil, err
	}
	if len(j.Segments) != len(s.Plan.Segments) {
		j.Segments = make([]SegCheckpoint, len(s.Plan.Segments))
		for i := range j.Segments {
			j.Segments[i].Index = i
		}
	}
	// 找出首个未完成段；之前的段必须全部完成，否则视为状态非法。
	pending := []int{}
	for i, seg := range s.Plan.Segments {
		cp := j.Segments[i]
		src := s.Frames[seg.StartFrame : seg.StartFrame+seg.Frames]
		if cp.Status == "done" && cp.Checksum == ChecksumFrames(src) {
			continue
		}
		pending = append(pending, i)
	}
	if len(pending) == 0 {
		return j, nil
	}

	type outcome struct {
		idx int
		cp  SegCheckpoint
		err error
	}
	jobs := make(chan int)
	results := make(chan outcome, len(pending))
	workers := s.Workers
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				cp, err := s.encodeSegment(idx)
				results <- outcome{idx: idx, cp: cp, err: err}
			}
		}()
	}
	go func() {
		for _, idx := range pending {
			jobs <- idx
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	var firstErr error
	for out := range results {
		if out.err != nil {
			if firstErr == nil {
				firstErr = out.err
			}
			j.Segments[out.idx] = out.cp
			continue
		}
		j.Segments[out.idx] = out.cp
		if s.OnSegmentDone != nil {
			s.OnSegmentDone(out.cp)
		}
	}
	j.State = s.Machine.State()
	if err := Save(s.JournalPath, j); err != nil {
		return j, fmt.Errorf("写检查点失败: %w", err)
	}
	if firstErr != nil {
		return j, firstErr
	}
	return j, nil
}

// encodeSegment 编码单个段，失败按次数重试，超限标记 failed。
func (s *Scheduler) encodeSegment(idx int) (SegCheckpoint, error) {
	seg := s.Plan.Segments[idx]
	src := s.Frames[seg.StartFrame : seg.StartFrame+seg.Frames]
	cp := SegCheckpoint{Index: idx, Checksum: ChecksumFrames(src)}
	var lastErr error
	for attempt := 0; attempt <= s.MaxSegRetry; attempt++ {
		res, qp, vbvMin, ok := func() (res *codec.SegResult, qp float64, vbvMin float64, ok bool) {
			defer func() {
				if r := recover(); r != nil {
					lastErr = fmt.Errorf("段 %d 编码 panic: %v", idx, r)
				}
			}()
			r, q, v, good := s.Controller.EncodeSegment(s.Frames, seg, s.Plan.BitrateBps, s.Plan.FPS)
			return r, q, v, good
		}()
		if lastErr != nil {
			continue // 编码本身失败，重试
		}
		cp.QP = qp
		cp.Bits = res.Bits
		cp.FrameBits = res.FrameBits
		cp.VBVMinRatio = vbvMin
		// 用重建帧算逐帧 PSNR
		recon := codec.EncodeSegment(src, qp, true)
		cp.FramePSNR = codec.SegmentPSNR(src, recon.Recon)
		cp.PSNRMeanDB, cp.PSNRStdDB = codec.MeanStd(cp.FramePSNR)
		if !ok {
			cp.Status = "unreachable"
			return cp, fmt.Errorf("段 %d 不可达：目标 %d 比特，最优偏差 %.2f%%", idx, seg.TargetBits, rc.DeviationPct(res.Bits, seg.TargetBits))
		}
		cp.Status = "done"
		return cp, nil
	}
	cp.Status = "failed"
	return cp, fmt.Errorf("段 %d 编码失败（已重试 %d 次）: %w", idx, s.MaxSegRetry, lastErr)
}
