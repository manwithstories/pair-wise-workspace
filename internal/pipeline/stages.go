package pipeline

import (
	"hash/crc32"

	"bcastqc/internal/analyze"
	"bcastqc/internal/graph"
	"bcastqc/internal/ingest"
	"bcastqc/internal/model"
	"bcastqc/internal/repack"
)

// runAnalyze 执行 WAV 结构核验、包窗口组装与损坏定位。
func (r *Runner) runAnalyze(fix *Fixture) (
	[]analyze.StructureReport,
	[]*analyze.DamageReport,
	[]*model.TrackTimeline,
	error,
) {
	structReps := make([]analyze.StructureReport, 0, len(fix.Tracks))
	damageReps := make([]*analyze.DamageReport, 0, len(fix.Tracks))
	timelines := make([]*model.TrackTimeline, 0, len(fix.Tracks))

	for _, tf := range fix.Tracks {
		clock, err := model.NewClock(sampleRate)
		if err != nil {
			return nil, nil, nil, err
		}
		tl, err := model.NewTrackTimeline(tf.ID, clock, tf.ExpectFrames)
		if err != nil {
			return nil, nil, nil, err
		}

		// 结构核验：WAV 帧数应与期望采样总数一致（截断故障会在此显形）。
		srep, err := analyze.AnalyzeStructure(tf.ID, tf.Info, tf.ExpectFrames)
		if err != nil {
			return nil, nil, nil, err
		}
		structReps = append(structReps, *srep)

		// 包序整理：去重、冲突检测、乱序窗口与突发丢包判定。
		recs, err := assemble(tf, r.cfg.ReorderWindow)
		if err != nil {
			return nil, nil, nil, err
		}

		// 登记同步锚点（PCM 关键帧标记完全来自包记录）。
		for _, rec := range recs {
			if rec.Anchor {
				tl.AddAnchor(model.Anchor{TrackID: tf.ID, Sample: rec.StartSample, Seq: rec.Seq})
			}
		}

		// 损坏定位：联合 CRC、采样连续性与 WAV 长度。
		drep, err := analyze.LocateDamage(tf.ID, recs, tl, tf.ExpectFrames)
		if err != nil {
			return nil, nil, nil, err
		}
		damageReps = append(damageReps, drep)
		timelines = append(timelines, tl)
	}
	return structReps, damageReps, timelines, nil
}

// assemble 对单轨包记录执行窗口整理并返回有序记录。
func assemble(tf *TrackFixture, window int64) ([]ingest.PacketRecord, error) {
	asm := ingest.NewWindowAssembler(tf.ID, window)
	for _, p := range tf.Packets {
		if err := asm.AddPacket(p); err != nil {
			return nil, err
		}
	}
	recs, _ := asm.Flush()
	return recs, nil
}

// runGraph 构建显式轨道依赖图，核验拓扑、环路、漂移与越界。
func (r *Runner) runGraph(fix *Fixture, timelines []*model.TrackTimeline) error {
	plan := graph.NewPlan(r.cfg.DriftTolerance)
	for _, tf := range fix.Tracks {
		node := graph.TrackNode{
			ID:           tf.ID,
			SampleRate:   sampleRate,
			TotalSamples: tf.ExpectFrames,
		}
		if err := plan.AddNode(node); err != nil {
			return err
		}
	}
	// 正常轨道采用线性依赖链：i 依赖 i-1，保证确定性拓扑序。
	for i := 1; i < len(fix.Tracks); i++ {
		if err := plan.AddEdge(i, i-1); err != nil {
			return err
		}
	}
	order, err := plan.TopoSort()
	if err != nil {
		return err
	}
	if len(order) != len(fix.Tracks) {
		return model.ErrUnrecoverable
	}
	if err := plan.CheckClockDrift(); err != nil {
		return err
	}
	// 越界采样点核验：所有缺口端点必须落在轨道声明范围内。
	for _, tl := range timelines {
		for _, g := range tl.Gaps {
			if err := plan.CheckSampleRange(int64(tl.TrackID), g.Start); err != nil {
				return err
			}
			if err := plan.CheckSampleRange(int64(tl.TrackID), g.End); err != nil {
				return err
			}
		}
	}

	// 依赖成环样例：独立建图，必须被环路检测拒绝。
	if len(fix.CycleEdges) > 0 {
		cyclic := graph.NewPlan(r.cfg.DriftTolerance)
		for _, tf := range fix.Tracks {
			_ = cyclic.AddNode(graph.TrackNode{
				ID: tf.ID, SampleRate: sampleRate, TotalSamples: tf.ExpectFrames,
			})
		}
		for _, e := range fix.CycleEdges {
			if err := cyclic.AddEdge(e[0], e[1]); err != nil {
				return err
			}
		}
		if _, err := cyclic.TopoSort(); err == nil {
			return model.ErrUnrecoverable
		}
	}
	return nil
}

// runRepack 按时间线把各轨道交织写出为母版，缺口固定补零并生成清单。
func (r *Runner) runRepack(fix *Fixture, timelines []*model.TrackTimeline) (*repack.Manifest, error) {
	chans := len(fix.Tracks)
	totalFrames := fix.Tracks[0].ExpectFrames
	w, err := repack.NewWriter(r.cfg.OutDir, sampleRate, 16, chans, totalFrames)
	if err != nil {
		return nil, err
	}

	trackData, err := r.buildTrackPCM(fix)
	if err != nil {
		return nil, err
	}

	// 逐帧交织：缺口帧自然取零值，同时在每个缺口起点登记一条清单。
	manifestLogged := make([]map[int64]bool, chans)
	for i := range manifestLogged {
		manifestLogged[i] = map[int64]bool{}
	}
	const blockFrames = 2048
	block := make([]int, 0, blockFrames*int64(chans))

	for frame := int64(0); frame < totalFrames; frame++ {
		for ch, tl := range timelines {
			val := 0
			if !gapAt(tl, frame) {
				val = trackData[ch][frame]
			}
			block = append(block, val)
			for _, g := range tl.Gaps {
				if g.Start == frame && !manifestLogged[ch][frame] {
					manifestLogged[ch][frame] = true
					// 只登记清单；零采样已由上面的交织主流逐帧写出。
					if err := w.RecordZeroFill(ch, g.Start, g.End, g.Kind, g.LocError); err != nil {
						return nil, err
					}
				}
			}
		}
		if len(block) == cap(block) || frame == totalFrames-1 {
			if err := w.WriteFrames(block); err != nil {
				return nil, err
			}
			block = block[:0]
		}
	}
	return w.Close()
}

// buildTrackPCM 组装每轨道完好区间的 16bit 采样（损坏处置零），按帧索引访问。
func (r *Runner) buildTrackPCM(fix *Fixture) ([][]int, error) {
	out := make([][]int, len(fix.Tracks))
	table := crc32.MakeTable(crc32.IEEE)
	for ti, tf := range fix.Tracks {
		samples := make([]int, tf.ExpectFrames)
		recs, err := assemble(tf, r.cfg.ReorderWindow)
		if err != nil {
			return nil, err
		}
		for _, rec := range recs {
			// CRC 失败的包不参与重建，对应帧保持零值。
			if rec.CRC != 0 && crc32.Checksum(rec.Payload, table) != rec.CRC {
				continue
			}
			for i := int64(0); i < rec.Frames; i++ {
				idx := rec.StartSample + i
				if idx >= tf.ExpectFrames || int(idx*2+1) >= len(rec.Payload) {
					continue
				}
				samples[idx] = int(int16(
					uint16(rec.Payload[idx*2]) | uint16(rec.Payload[idx*2+1])<<8,
				))
			}
		}
		out[ti] = samples
	}
	return out, nil
}

// gapAt 判断某帧是否落在时间线的任一缺口内。
func gapAt(tl *model.TrackTimeline, frame int64) bool {
	for _, g := range tl.Gaps {
		if g.Contains(frame) {
			return true
		}
	}
	return false
}
