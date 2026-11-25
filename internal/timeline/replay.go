package timeline

import (
	"fmt"

	"mediaarchive/internal/clock"
	"mediaarchive/internal/fault"
	"mediaarchive/internal/ingest"
)

// DefaultReplayCheckpointInterval 在完整运行中周期性建立内存检查点。
const DefaultReplayCheckpointInterval = 1000

// Checkpoint 是回放器可使用的最近状态快照。
type Checkpoint struct {
	Cursor int
	State  State
}

// Replayer 按输入序号处理事件，并在跨水位迟到时从最近检查点确定性回放。
type Replayer struct {
	State       State
	Events      []ingest.Packet
	Checkpoints []Checkpoint
	Allowed     clock.Ticks
	Interval    int
}

// NewReplayer 创建回放器。events 必须按全局输入序号排列。
func NewReplayer(base State, events []ingest.Packet, allowed clock.Ticks) *Replayer {
	if allowed <= 0 {
		allowed = DefaultAllowedLateness
	}
	r := &Replayer{
		State:       base,
		Events:      events,
		Checkpoints: []Checkpoint{{Cursor: base.Cursor, State: base.Clone()}},
		Allowed:     allowed,
		Interval:    DefaultReplayCheckpointInterval,
	}
	return r
}

// ProcessAll 处理到 targetCursor（含）；targetCursor=0 表示处理全部事件。
func (r *Replayer) ProcessAll(targetCursor int) error {
	for i := r.State.Cursor; i < len(r.Events); i++ {
		p := r.Events[i]
		if p.No != i+1 {
			return fmt.Errorf("timeline: event order mismatch at index %d: no=%d", i, p.No)
		}
		if err := r.Process(p); err != nil {
			return err
		}
		if targetCursor > 0 && r.State.Cursor >= targetCursor {
			return nil
		}
	}
	return nil
}

// Process 处理单个事件，必要时执行检查点回放。
func (r *Replayer) Process(p ingest.Packet) error {
	wm := NewWatermark(r.Allowed)
	wm.Values = r.State.Watermarks
	end := End(p)

	if wm.IsLate(p.TrackID, end) {
		cp := r.nearestCheckpoint(p.No - 1)
		replayed := cp.State.Clone()
		startIdx := cp.Cursor
		if startIdx < 0 {
			startIdx = 0
		}
		if startIdx > p.No {
			return fault.New(fault.KindCheckpoint, "检查点游标超过待回放事件", nil)
		}
		// p.No 为 1-based；切片终点 p.No 恰好包含当前事件（索引 p.No-1）。
		replayed = Reduce(replayed, r.Events[startIdx:p.No])

		impact := fault.LateImpact{
			Track:            p.TrackID,
			ExtSeq:           uint64(p.ExtSeq),
			EventEndTicks:    int64(end),
			WatermarkTicks:   r.State.Watermarks[p.TrackID],
			CheckpointCursor: cp.Cursor,
			ReplayedFrom:     cp.Cursor,
			ReplayedTo:       p.No,
			AffectedFrames:   affectedFrames(p.Start, end, clock.Hz),
		}
		if ref := sourceLocation(p.Source, p.TrackID); ref != nil {
			impact.Location = ref
		}
		replayed.LateCount++
		replayed.ReplayCount++
		replayed.LateEvents = append(replayed.LateEvents, impact)
		replayed.Findings = append(replayed.Findings, fault.Finding{
			Code: "late_behind_watermark_replayed", Severity: "late", Recovered: true,
			Message:  "迟到事件低于水位容忍线，已从最近检查点回放，最终状态与完整重跑一致",
			Location: impact.Location,
		})
		r.State = replayed
		r.addCheckpointIfDue(p.No, true)
		return nil
	}

	r.State.Apply(p)
	r.addCheckpointIfDue(p.No, false)
	return nil
}

// Reduce 是纯确定性归约：从 base 状态依次应用 events。
func Reduce(base State, events []ingest.Packet) State {
	s := base.Clone()
	for _, p := range events {
		s.Apply(p)
	}
	return s
}

func (r *Replayer) nearestCheckpoint(cursor int) Checkpoint {
	best := r.Checkpoints[0]
	for _, cp := range r.Checkpoints {
		if cp.Cursor <= cursor && cp.Cursor >= best.Cursor {
			best = cp
		}
	}
	return best
}

func (r *Replayer) addCheckpointIfDue(cursor int, force bool) {
	if !force && (r.Interval <= 0 || cursor%r.Interval != 0) {
		return
	}
	cp := Checkpoint{Cursor: cursor, State: r.State.Clone()}
	// 同一游标只保留一份；周期性检查点保留最近若干个即可。
	for i := len(r.Checkpoints) - 1; i >= 0; i-- {
		if r.Checkpoints[i].Cursor == cursor {
			r.Checkpoints[i] = cp
			r.trimCheckpoints()
			return
		}
	}
	r.Checkpoints = append(r.Checkpoints, cp)
	r.trimCheckpoints()
}

func (r *Replayer) trimCheckpoints() {
	// 12k 事件、每 1k 一个快照时保留全部也很小；这里限制为最近 16 个。
	const max = 16
	if len(r.Checkpoints) <= max {
		return
	}
	// 始终保留索引 0 的初始检查点。
	keep := make([]Checkpoint, 0, max)
	keep = append(keep, r.Checkpoints[0])
	keep = append(keep, r.Checkpoints[len(r.Checkpoints)-(max-1):]...)
	r.Checkpoints = keep
}

func affectedFrames(start, end clock.Ticks, interval int64) []int {
	if interval <= 0 || end <= start {
		return nil
	}
	first := int(int64(start) / interval)
	last := int((int64(end) - 1) / interval)
	frames := make([]int, 0, last-first+1)
	for f := first; f <= last; f++ {
		frames = append(frames, f)
	}
	return frames
}

func sourceLocation(ref ingest.SourceRef, track string) *fault.Location {
	if ref.Path == "" {
		return nil
	}
	return &fault.Location{Source: ref.Path, Offset: ref.Offset, Length: int64(ref.Length), Box: ref.Box, Track: track, Sample: ref.SampleIndex}
}
