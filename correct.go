package main

import (
	"fmt"
	"math/big"
	"sort"
)

// AnalyzeOptions 是 analyze 子命令的全部输入。
type AnalyzeOptions struct {
	InPath  string
	JSONOut string
	CSVOut  string
	Target  Timebase // 校正后导出时间基，默认 1/1e9（纳秒）
}

// Summary 是终端输出与测试共享的拟合汇总。
type Summary struct {
	Fits           map[int64]*Fit
	Epochs         []int64 // 升序 epoch，即段号顺序
	CorrectedCount int
}

type anchorPoint struct {
	line  int
	ns    int64
	epoch int64
}

type pass1 struct {
	uw      *Unwrapper
	video   map[string]anchorPoint
	audio   map[string]anchorPoint
	idOrder []string
	hasV    bool
	hasA    bool
	epochsV map[int64]bool
	epochsA map[int64]bool
	lastSeq map[string]int64 // (track/epoch,seq 行) 去重
	seqLine map[string]int
}

// Analyze 执行两遍流式分析。第一遍解析、解绕并按同步编号配对锚点；
// 逐段拟合通过后才打开导出文件，第二遍重放输入生成校正序列。
// 百万级记录下常驻内存仅与锚点数/epoch 数成正比。
func Analyze(opts AnalyzeOptions) (*Summary, error) {
	p1, err := firstPassScan(opts.InPath)
	if err != nil {
		return nil, err
	}
	if !p1.hasV || !p1.hasA {
		missing := "video"
		if p1.hasV {
			missing = "audio"
		}
		return nil, fmt.Errorf("%w: missing required track: %s", ErrInput, missing)
	}
	if !sameEpochs(p1.epochsV, p1.epochsA) {
		return nil, fmt.Errorf("%w: epoch sets differ between audio and video tracks; restarts must occur on both tracks", ErrInput)
	}
	// 按 epoch 分组配对；同步编号在两条轨上必须同时存在，否则配对失败。
	byEpoch := make(map[int64][]Pair)
	for _, id := range p1.idOrder {
		v, vok := p1.video[id]
		a, aok := p1.audio[id]
		if !vok || !aok {
			line := v.line
			if aok {
				line = a.line
			}
			return nil, lineError(line, "sync_id %q present on only one track", id)
		}
		if v.epoch != a.epoch {
			return nil, lineError(v.line, "sync_id %q crosses epochs (video e%d / audio e%d)", id, v.epoch, a.epoch)
		}
		byEpoch[v.epoch] = append(byEpoch[v.epoch], Pair{
			SyncID: id, LineV: int64(v.line), LineA: int64(a.line),
			Video: v.ns, Audio: a.ns,
		})
	}
	epochs := sortedEpochs(p1.epochsV)
	fits := make(map[int64]*Fit, len(epochs))
	for _, ep := range epochs {
		f, err := FitSegment(ep, byEpoch[ep])
		if err != nil {
			return nil, err
		}
		fits[ep] = f
	}

	// 拟合全部成功后才创建导出器；任何失败都不会留下半成品文件。
	var jw *JSONWriter
	var cw *CSVWriter
	cleanup := func() {
		if jw != nil {
			jw.Abort()
		}
		if cw != nil {
			cw.Abort()
		}
	}
	if opts.JSONOut != "" {
		jw, err = NewJSONWriter(opts.JSONOut)
		if err != nil {
			return nil, err
		}
	}
	if opts.CSVOut != "" {
		cw, err = NewCSVWriter(opts.CSVOut)
		if err != nil {
			cleanup()
			return nil, err
		}
	}

	n, err := secondPassCorrect(opts, p1, fits, epochs, jw, cw)
	if err != nil {
		cleanup()
		return nil, err
	}
	if jw != nil {
		if err := jw.Close(); err != nil {
			cw.Abort()
			return nil, err
		}
	}
	if cw != nil {
		if err := cw.Close(); err != nil {
			return nil, err
		}
	}
	return &Summary{Fits: fits, Epochs: epochs, CorrectedCount: n}, nil
}

func firstPassScan(path string) (*pass1, error) {
	f, closeFn, err := openInput(path)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	p := &pass1{
		uw:      NewUnwrapper(),
		video:   make(map[string]anchorPoint),
		audio:   make(map[string]anchorPoint),
		epochsV: make(map[int64]bool),
		epochsA: make(map[int64]bool),
		lastSeq: make(map[string]int64),
		seqLine: make(map[string]int),
	}
	err = ScanRecords(f, func(r *Record) error {
		// 同一 (track, epoch) 内 seq 不得重复。
		sk := uwKey(r.Track, r.Epoch)
		if ln, ok := p.seqLine[sk+fmt.Sprint(r.Seq)]; ok {
			return lineError(r.Line, "duplicate seq=%d on track=%s epoch=%d (first at line %d)",
				r.Seq, r.Track, r.Epoch, ln)
		}
		p.seqLine[sk+fmt.Sprint(r.Seq)] = r.Line

		unw, err := p.uw.Unwrap(r)
		if err != nil {
			return err
		}
		ns, err := ticksToNs(unw, r.TB.Num, r.TB.Den)
		if err != nil {
			return lineError(r.Line, "unwrapped pts overflows int64 in nanoseconds: %v", err)
		}
		if r.Track == "video" {
			p.hasV = true
			p.epochsV[r.Epoch] = true
		} else {
			p.hasA = true
			p.epochsA[r.Epoch] = true
		}
		if r.HasSync {
			ap := anchorPoint{line: r.Line, ns: ns, epoch: r.Epoch}
			if r.Track == "video" {
				if old, dup := p.video[r.SyncID]; dup {
					return lineError(r.Line, "duplicate sync_id %q on video (first at line %d)", r.SyncID, old.line)
				}
				p.video[r.SyncID] = ap
			} else {
				if old, dup := p.audio[r.SyncID]; dup {
					return lineError(r.Line, "duplicate sync_id %q on audio (first at line %d)", r.SyncID, old.line)
				}
				p.audio[r.SyncID] = ap
			}
			p.idOrder = append(p.idOrder, r.SyncID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// secondPassCorrect 重放输入：视频作为参考钟，音频按 (a-β)/α 校正；
// 保留负时间与可变帧率间隔，不做任何补帧。两条导出路径复用同一
// timebase 换算函数，逐项结果严格一致。
func secondPassCorrect(opts AnalyzeOptions, p1 *pass1, fits map[int64]*Fit,
	epochs []int64, jw *JSONWriter, cw *CSVWriter) (int, error) {
	f, closeFn, err := openInput(opts.InPath)
	if err != nil {
		return 0, err
	}
	defer closeFn()
	uw := NewUnwrapper()
	segOrd := make(map[int64]int, len(epochs))
	for i, ep := range epochs {
		segOrd[ep] = i
	}
	count := 0
	err = ScanRecords(f, func(r *Record) error {
		unw, err := uw.Unwrap(r)
		if err != nil {
			return err
		}
		sec := ticksToRat(unw, r.TB.Num, r.TB.Den)
		var correctedSec *big.Rat
		if r.Track == "video" {
			correctedSec = new(big.Rat).Set(sec)
		} else {
			f := fits[r.Epoch]
			correctedSec = new(big.Rat).Sub(sec, f.Beta)
			correctedSec.Quo(correctedSec, f.Alpha)
		}
		ticks, err := secondsToTargetTicks(correctedSec, opts.Target.Num, opts.Target.Den)
		if err != nil {
			return lineError(r.Line, "corrected timestamp overflows int64: %v", err)
		}
		row := OutputRow{
			Track:        r.Track,
			Segment:      segOrd[r.Epoch],
			Epoch:        r.Epoch,
			Seq:          r.Seq,
			OriginalPTS:  r.PTS,
			CorrectedPTS: ticks,
			SyncID:       r.SyncID,
			HasSync:      r.HasSync,
		}
		if jw != nil {
			if err := jw.Write(row); err != nil {
				return err
			}
		}
		if cw != nil {
			if err := cw.Write(row); err != nil {
				return err
			}
		}
		count++
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

func sortedEpochs(set map[int64]bool) []int64 {
	out := make([]int64, 0, len(set))
	for ep := range set {
		out = append(out, ep)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sameEpochs(a, b map[int64]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
