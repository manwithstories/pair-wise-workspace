package pipeline

import (
	"strconv"

	"medpipe/internal/clock"
	"medpipe/internal/index"
	"medpipe/internal/ingest"
	"medpipe/internal/manifest"
)

// processBlocks 逐块读取、模拟弱网、推送重排并提交到时间线。
func (r *Runner) processBlocks(idx *index.Index, ro *ingest.Reorder, st *ingest.Streamer,
	cp *Checkpoint, startBlock int64, res *Result) error {
	processed := 0
	for _, bi := range idx.Blocks {
		if bi.ID < startBlock {
			// resume 场景下这些块已由 replay 推进过 streamer 坐标，
			// 这里绝不能再 Skip，否则包序号会被二次前推。
			continue
		}
		pkts, missing, err := st.ReadBlock(bi)
		if err != nil {
			return err
		}
		arrive, _, nstats, err := ingest.Simulate(pkts, ingest.FaultPlan{
			Mode: r.opts.Fault, Seed: r.opts.Seed + bi.ID, WindowSize: r.window,
		})
		if err != nil {
			return err
		}
		res.Network = mergeStats(res.Network, nstats)
		// 块内先全部入队：原包与冲突包同处 pending 时即可检出内容冲突。
		for _, p := range arrive {
			err := ro.Push(p)
			if clock.CodeOf(err) == clock.CodeConflict {
				return clock.NewError(clock.CodeConflict, bi.ID,
					"块 %d 包 %d 内容冲突，拒绝生成清单", bi.ID, p.Seq)
			}
			// 窗口外迟到包不致命：跳过，绝不改写已提交样本。
			if clock.CodeOf(err) == clock.CodeLatePacket {
				continue
			}
			if err != nil {
				return err
			}
		}
		// 入队完成后统一提交连续包。
		for _, done := range ro.Drain() {
			if err := r.commit(done, cp); err != nil {
				return err
			}
		}
		// 块末：本块缺失包不会再到达，按静音补齐并推进水位，避免下一块越窗。
		blockEndSeq := (st.FramePos() + r.opts.PacketFrames - 1) / r.opts.PacketFrames
		filled, err := ro.FlushThrough(blockEndSeq)
		if err != nil {
			return err
		}
		for _, p := range filled {
			if err := r.commit(p, cp); err != nil {
				return err
			}
		}
		r.recordBlock(bi, missing)
		cp.LastBlock = bi.ID
		cp.BlockDigests[strconv.FormatInt(bi.ID, 10)] = bi.SHA256
		processed++
		if r.opts.StopAfter > 0 && processed >= r.opts.StopAfter {
			cp.Interrupted = true
			if err := cp.save(r.opts.OutDir); err != nil {
				return err
			}
			res.Interrupted = true
			return clock.NewError(clock.CodeInterrupted, bi.ID,
				"已处理 %d 块，保留检查点后中断", processed)
		}
	}
	return nil
}

func (r *Runner) commit(p *ingest.Packet, cp *Checkpoint) error {
	if err := r.mixer.Apply(p); err != nil {
		return err
	}
	cp.fold(ingest.PacketSHA(p), SavedPacket{
		Seq: p.Seq, BlockID: p.BlockID, StartFrame: p.StartFrame, Frames: p.Frames,
		Source: string(p.Source), SHA256: p.SHA256, PCM: append([]byte(nil), p.PCM...),
	})
	return nil
}

func (r *Runner) recordBlock(bi index.BlockIndex, missingFrames int64) {
	r.blocks = append(r.blocks, manifest.BlockRecord{
		ID: bi.ID, SHA256: bi.SHA256, Frames: bi.Frames,
		MissingFrames: bi.MissingFrames, Truncated: bi.Truncated,
		Compensated:    missingFrames * int64(bi.Channels),
		TimeAlignError: 0,
	})
}

func (r *Runner) drainAndFill(idx *index.Index, ro *ingest.Reorder, cp *Checkpoint) error {
	totalPkts := (r.mixer.TotalFrames() + r.opts.PacketFrames - 1) / r.opts.PacketFrames
	out, err := ro.FlushThrough(totalPkts)
	if err != nil {
		return err
	}
	for _, p := range out {
		if err := r.commit(p, cp); err != nil {
			return err
		}
	}
	return nil
}

// replay 在 resume 时按检查点保存的已提交包原样重建 mixer 与重排水位，
// 不重新模拟故障、不重复 fold，保证与中断前逐字节一致。
func (r *Runner) replay(cp *Checkpoint, idx *index.Index, ro *ingest.Reorder, st *ingest.Streamer) error {
	for _, bi := range idx.Blocks {
		if bi.ID > cp.LastBlock {
			break
		}
		if got := cp.BlockDigests[strconv.FormatInt(bi.ID, 10)]; got != bi.SHA256 {
			return clock.NewError(clock.CodeChecksum, bi.ID, "块 %d 摘要与检查点不符", bi.ID)
		}
	}
	for _, sp := range cp.Packets {
		p := &ingest.Packet{
			Seq: sp.Seq, BlockID: sp.BlockID, StartFrame: sp.StartFrame, Frames: sp.Frames,
			Source: ingest.Source(sp.Source), SHA256: sp.SHA256, PCM: sp.PCM,
		}
		if err := r.mixer.Apply(p); err != nil {
			return err
		}
		if _, err := ro.FlushThrough(sp.Seq + 1); err != nil {
			return err
		}
	}
	histFrames := int64(0)
	for _, bi := range idx.Blocks {
		if bi.ID > cp.LastBlock {
			break
		}
		histFrames += bi.Frames + bi.MissingFrames
		r.recordBlock(bi, bi.MissingFrames)
	}
	st.Skip(histFrames)
	return nil
}

func mergeStats(a, b ingest.NetworkStats) ingest.NetworkStats {
	a.Dropped = append(a.Dropped, b.Dropped...)
	a.Duplicates = append(a.Duplicates, b.Duplicates...)
	a.Conflicts = append(a.Conflicts, b.Conflicts...)
	a.Reordered += b.Reordered
	if b.MinBPS != 0 && (a.MinBPS == 0 || b.MinBPS < a.MinBPS) {
		a.MinBPS = b.MinBPS
	}
	if b.MaxBPS > a.MaxBPS {
		a.MaxBPS = b.MaxBPS
	}
	return a
}
