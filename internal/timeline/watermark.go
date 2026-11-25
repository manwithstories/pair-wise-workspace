// Package timeline 实现去重、空洞、水位、迟到回放和确定性归并。
package timeline

import (
	"sort"
	"strconv"

	"mediaarchive/internal/clock"
	"mediaarchive/internal/fault"
	"mediaarchive/internal/ingest"
)

// DefaultAllowedLateness 给普通乱序保留 1 秒容忍窗口。
const DefaultAllowedLateness clock.Ticks = 48_000

// Watermark 按轨道记录已观察到的最大事件结束时间。
type Watermark struct {
	Allowed clock.Ticks      `json:"allowed_ticks"`
	Values  map[string]int64 `json:"values"`
}

// NewWatermark 创建水位跟踪器。
func NewWatermark(allowed clock.Ticks) *Watermark {
	if allowed <= 0 {
		allowed = DefaultAllowedLateness
	}
	return &Watermark{Allowed: allowed, Values: map[string]int64{}}
}

// Level 返回轨道水位。
func (w *Watermark) Level(track string) clock.Ticks {
	return clock.Ticks(w.Values[track])
}

// IsLate 判断事件结束时间是否明显落在水位容忍线之后。
func (w *Watermark) IsLate(track string, end clock.Ticks) bool {
	level := w.Level(track)
	_, seen := w.Values[track]
	return seen && end+w.Allowed < level
}

// Observe 用事件时间推进水位，返回是否推进及新水位。
func (w *Watermark) Observe(track string, end clock.Ticks) (bool, clock.Ticks) {
	old := w.Level(track)
	if end > old {
		w.Values[track] = int64(end)
		return true, end
	}
	return false, old
}

// State 是可序列化、可哈希、可从检查点恢复的确定性时间线状态。
type State struct {
	Cursor int `json:"cursor"`

	// Seen 保存身份键到内容哈希；相同哈希重试幂等。
	Seen map[string]string `json:"seen"`
	// SeenRef 保存首次来源，供冲突定位。
	SeenRef map[string]ingest.SourceRef `json:"seen_ref"`
	// PacketIndex 指向 Packets 中的规范包。
	PacketIndex map[string]int `json:"packet_index"`
	// Quarantined 标记身份相同但内容冲突的键。
	Quarantined map[string]bool `json:"quarantined"`

	Packets []ingest.Packet `json:"packets"`

	Watermarks map[string]int64 `json:"watermarks"`
	MaxExtSeq  map[string]int64 `json:"max_ext_seq"`
	HasExtSeq  map[string]bool  `json:"has_ext_seq"`

	Holes      []fault.Hole       `json:"holes"`
	Conflicts  []fault.Conflict   `json:"conflicts"`
	LateEvents []fault.LateImpact `json:"late_events"`
	Findings   []fault.Finding    `json:"findings"`

	AcceptedCount  int64 `json:"accepted_count"`
	DuplicateCount int64 `json:"duplicate_count"`
	ConflictCount  int64 `json:"conflict_count"`
	LateCount      int64 `json:"late_count"`
	ReplayCount    int64 `json:"replay_count"`
}

// NewState 创建初始化状态。
func NewState() State {
	return State{
		Seen:        map[string]string{},
		SeenRef:     map[string]ingest.SourceRef{},
		PacketIndex: map[string]int{},
		Quarantined: map[string]bool{},
		Watermarks:  map[string]int64{},
		MaxExtSeq:   map[string]int64{},
		HasExtSeq:   map[string]bool{},
	}
}

func (s *State) ensureMaps() {
	if s.Seen == nil {
		s.Seen = map[string]string{}
	}
	if s.SeenRef == nil {
		s.SeenRef = map[string]ingest.SourceRef{}
	}
	if s.PacketIndex == nil {
		s.PacketIndex = map[string]int{}
	}
	if s.Quarantined == nil {
		s.Quarantined = map[string]bool{}
	}
	if s.Watermarks == nil {
		s.Watermarks = map[string]int64{}
	}
	if s.MaxExtSeq == nil {
		s.MaxExtSeq = map[string]int64{}
	}
	if s.HasExtSeq == nil {
		s.HasExtSeq = map[string]bool{}
	}
}

// Clone 深拷贝可序列化状态，用于检查点和回放快照。
func (s State) Clone() State {
	out := s
	out.Seen = cloneStringMap(s.Seen)
	out.SeenRef = cloneRefMap(s.SeenRef)
	out.PacketIndex = cloneIntMap(s.PacketIndex)
	out.Quarantined = cloneBoolMap(s.Quarantined)
	out.Watermarks = cloneInt64Map(s.Watermarks)
	out.MaxExtSeq = cloneInt64Map(s.MaxExtSeq)
	out.HasExtSeq = cloneBoolMap(s.HasExtSeq)
	out.Packets = append([]ingest.Packet(nil), s.Packets...)
	out.Holes = append([]fault.Hole(nil), s.Holes...)
	out.Conflicts = append([]fault.Conflict(nil), s.Conflicts...)
	out.LateEvents = append([]fault.LateImpact(nil), s.LateEvents...)
	out.Findings = append([]fault.Finding(nil), s.Findings...)
	return out
}

func cloneStringMap(m map[string]string) map[string]string {
	n := make(map[string]string, len(m))
	for k, v := range m {
		n[k] = v
	}
	return n
}

func cloneRefMap(m map[string]ingest.SourceRef) map[string]ingest.SourceRef {
	n := make(map[string]ingest.SourceRef, len(m))
	for k, v := range m {
		n[k] = v
	}
	return n
}

func cloneIntMap(m map[string]int) map[string]int {
	n := make(map[string]int, len(m))
	for k, v := range m {
		n[k] = v
	}
	return n
}

func cloneBoolMap(m map[string]bool) map[string]bool {
	n := make(map[string]bool, len(m))
	for k, v := range m {
		n[k] = v
	}
	return n
}

func cloneInt64Map(m map[string]int64) map[string]int64 {
	n := make(map[string]int64, len(m))
	for k, v := range m {
		n[k] = v
	}
	return n
}

// Result 描述应用一个事件后的决策。
type Result struct {
	Identity  string
	Accepted  bool
	Duplicate bool
	Conflict  bool
	Late      bool
	Hole      *fault.Hole
}

// End 返回包的事件结束时间。
func End(p ingest.Packet) clock.Ticks { return p.Start + p.Duration }

// Apply 按身份去重、内容冲突隔离、扩展序号空洞和事件时间水位更新状态。
// 迟到回放由 Replayer 负责；此函数只做确定性归约。
func (s *State) Apply(p ingest.Packet) Result {
	s.ensureMaps()
	s.Cursor = p.No
	id := p.Identity()
	res := Result{Identity: id}

	if oldHash, ok := s.Seen[id]; ok {
		if oldHash == p.Hash {
			s.DuplicateCount++
			res.Duplicate = true
			if wm := s.Watermarks[p.TrackID]; int64(End(p)) > wm {
				s.Watermarks[p.TrackID] = int64(End(p))
			}
			return res
		}

		s.ConflictCount++
		res.Conflict = true
		s.Quarantined[id] = true
		firstRef := s.SeenRef[id]
		c := fault.Conflict{
			Track:     p.TrackID,
			ExtSeq:    uint64(p.ExtSeq),
			FirstHash: oldHash,
			OtherHash: p.Hash,
			Locations: []fault.Location{*sourceToLocation(firstRef, p.TrackID), *sourceToLocation(p.Source, p.TrackID)},
		}
		s.Conflicts = append(s.Conflicts, c)
		s.Findings = append(s.Findings, fault.Finding{
			Code: "identity_content_conflict", Severity: "quarantined", Recovered: true,
			Message:  "相同轨道和扩展序号但内容哈希冲突，已隔离身份",
			Location: sourceToLocation(p.Source, p.TrackID),
		})
		return res
	}

	// 新身份：先检测扩展序号缺口，再接受规范包。
	if s.HasExtSeq[p.TrackID] && p.ExtSeq > s.MaxExtSeq[p.TrackID]+1 {
		firstMissing := uint64(s.MaxExtSeq[p.TrackID] + 1)
		lastMissing := uint64(p.ExtSeq - 1)
		h := fault.Hole{
			Track:        p.TrackID,
			FirstMissing: firstMissing,
			LastMissing:  lastMissing,
			StartTicks:   s.holeStart(p.TrackID),
			EndTicks:     int64(p.Start),
			EndSource:    p.Source.Offset,
		}
		if idx, ok := s.PacketIndex[p.TrackID+"#"+strconv.FormatInt(s.MaxExtSeq[p.TrackID], 10)]; ok && idx < len(s.Packets) {
			prev := s.Packets[idx]
			h.StartTicks = int64(End(prev))
			h.StartSource = prev.Source.Offset
		}
		s.Holes = append(s.Holes, h)
		res.Hole = &h
		s.Findings = append(s.Findings, fault.Finding{
			Code: "sequence_hole", Severity: "missing", Recovered: false,
			Message:  "扩展序号存在丢包空洞；时间区间由相邻包事件时间确定",
			Location: &fault.Location{Track: p.TrackID, Packet: int64(firstMissing)},
		})
	}

	s.Seen[id] = p.Hash
	s.SeenRef[id] = p.Source
	s.PacketIndex[id] = len(s.Packets)
	s.Packets = append(s.Packets, p)
	s.AcceptedCount++
	res.Accepted = true

	if !s.HasExtSeq[p.TrackID] || p.ExtSeq > s.MaxExtSeq[p.TrackID] {
		s.MaxExtSeq[p.TrackID] = p.ExtSeq
		s.HasExtSeq[p.TrackID] = true
	}
	s.reconcileFilledHole(p)
	if int64(End(p)) > s.Watermarks[p.TrackID] {
		s.Watermarks[p.TrackID] = int64(End(p))
	}
	return res
}

func (s *State) holeStart(track string) int64 {
	// 若找不到最大序号包，使用当前水位作为保守边界；绝不按到达顺序猜时长。
	return s.Watermarks[track]
}

// reconcileFilledHole 在乱序包最终到达时闭合或缩小先前的临时序号缺口，
// 使最终 Holes 只保留真正未被填充的丢包区间。
func (s *State) reconcileFilledHole(p ingest.Packet) {
	seq := uint64(p.ExtSeq)
	for i := range s.Holes {
		h := s.Holes[i]
		if h.Track != p.TrackID || seq < h.FirstMissing || seq > h.LastMissing {
			continue
		}

		s.Findings = append(s.Findings, fault.Finding{
			Code: "out_of_order_gap_filled", Severity: "reordered", Recovered: true,
			Message:  "先前观察到的序号缺口已由乱序到达的包填充",
			Location: sourceToLocation(p.Source, p.TrackID),
		})

		switch {
		case seq == h.FirstMissing && seq == h.LastMissing:
			s.Holes = append(s.Holes[:i], s.Holes[i+1:]...)
		case seq == h.FirstMissing:
			s.Holes[i].FirstMissing++
			s.Holes[i].StartTicks = int64(End(p))
			s.Holes[i].StartSource = p.Source.Offset
		case seq == h.LastMissing:
			s.Holes[i].LastMissing--
			s.Holes[i].EndTicks = int64(p.Start)
			s.Holes[i].EndSource = p.Source.Offset
		default:
			left := h
			left.LastMissing = seq - 1
			left.EndTicks = int64(p.Start)
			left.EndSource = p.Source.Offset
			right := h
			right.FirstMissing = seq + 1
			right.StartTicks = int64(End(p))
			right.StartSource = p.Source.Offset
			tail := append([]fault.Hole(nil), s.Holes[i+1:]...)
			s.Holes = append(s.Holes[:i], left, right)
			s.Holes = append(s.Holes, tail...)
		}
		return
	}
}

// CanonicalPackets 返回排除冲突身份并按事件时间排序的包。
func (s State) CanonicalPackets() []ingest.Packet {
	out := make([]ingest.Packet, 0, len(s.Packets))
	for _, p := range s.Packets {
		if s.Quarantined[p.Identity()] {
			continue
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		if out[i].TrackID != out[j].TrackID {
			return out[i].TrackID < out[j].TrackID
		}
		if out[i].ExtSeq != out[j].ExtSeq {
			return out[i].ExtSeq < out[j].ExtSeq
		}
		return out[i].No < out[j].No
	})
	return out
}

func sourceToLocation(ref ingest.SourceRef, track string) *fault.Location {
	return &fault.Location{Source: ref.Path, Offset: ref.Offset, Length: int64(ref.Length), Box: ref.Box, Track: track, Sample: ref.SampleIndex}
}
