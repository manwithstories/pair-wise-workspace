package pipeline

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"

	"archivemedia/internal/audio"
	"archivemedia/internal/journal"
	"archivemedia/internal/media"
	"archivemedia/internal/packet"
	"archivemedia/internal/timeline"
)

// InterruptPlan 描述模拟中断（仅用于演示/测试）。
type InterruptPlan struct {
	Enabled    bool
	AtArrival  int
}

// Options 是一次流水线运行的输入。
type Options struct {
	Events     []*packet.Event
	ExtraData  []byte
	OutputWAV  string
	JournalDir string
	Resume     bool
	Interrupt  InterruptPlan
}

// Result 是运行结果与验收所需统计。
type Result struct {
	Manifest         *journal.Manifest
	OutputSHA256     string
	ResumeMS         int64
	ResumeMatched    bool
	DamagePacketDelta int
	Interrupted      bool
}

// Runner 执行一次完整恢复流水线。
//
// 两阶段模型：
//   - 阶段 A（ingest）：去重、序号展开、时间线重建与占位填充，纯事件处理，
//     可在任意到达点落检查点并中断；
//   - 阶段 B（render）：从确定的时间线解码、重采样、响度归一、写 WAV。
//
// 续跑时从检查点恢复已 ingest 的事件，再喂入剩余事件，重建出的时间线与
// 全新跑完全一致，因此渲染输出逐字节相同。
type Runner struct {
	opt       Options
	dedup     *packet.Deduper
	replayer  *timeline.Replayer
	watermark *timeline.Watermark
	bwe       *timeline.BandwidthEstimator
	decoder   *packet.Decoder
	resampler *audio.Resampler
	store     *journal.Store
	ingested  map[int64]*packet.Event // 展开序号 -> 规范事件
	stats     journal.Manifest
	// resumeFromArrival 是续跑起点（已处理到达数）；首跑为 0。
	resumeFromArrival int
	hadCheckpoint     bool
	debugNorm         *audio.NormalizeResult
	debugAfter        audio.LoudnessStats
}

// New 构造 Runner。
func New(opt Options) (*Runner, error) {
	if opt.OutputWAV == "" || opt.JournalDir == "" {
		return nil, media.E(media.KindArgument, "pipeline.new",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"输出路径与检查点目录必填", nil)
	}
	store, err := journal.NewStore(opt.JournalDir)
	if err != nil {
		return nil, err
	}
	r := &Runner{
		opt:       opt,
		dedup:     packet.NewDeduper(),
		bwe:       timeline.NewBandwidthEstimator(),
		ingested:  make(map[int64]*packet.Event),
		store:     store,
		stats:     *journal.NewManifest(),
	}
	r.watermark = timeline.NewWatermark(r.bwe)
	r.replayer = timeline.NewReplayer(48000)
	return r, nil
}

// inputDigest 对事件集合按键排序后做确定性摘要，与到达顺序无关。
func (r *Runner) inputDigest() string {
	keys := make([]packet.Key, 0, len(r.opt.Events))
	seen := map[packet.Key]bool{}
	for _, e := range r.opt.Events {
		if !seen[e.Key()] {
			seen[e.Key()] = true
			keys = append(keys, e.Key())
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.StreamID != b.StreamID {
			return a.StreamID < b.StreamID
		}
		if a.Epoch != b.Epoch {
			return a.Epoch < b.Epoch
		}
		return a.Seq < b.Seq
	})
	h := sha256.New()
	var buf [10]byte
	for _, k := range keys {
		buf[0] = byte(k.StreamID >> 24)
		buf[1] = byte(k.StreamID >> 16)
		buf[2] = byte(k.StreamID >> 8)
		buf[3] = byte(k.StreamID)
		buf[4] = byte(k.Epoch >> 24)
		buf[5] = byte(k.Epoch >> 16)
		buf[6] = byte(k.Epoch >> 8)
		buf[7] = byte(k.Epoch)
		buf[8] = byte(k.Seq >> 8)
		buf[9] = byte(k.Seq)
		h.Write(buf[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Run 执行流水线。中断时返回带 Interrupted=true 的结果与 KindTimeline 哨兵错误；
// 调用方可在续跑时重新构造 Runner 并以 Resume=true 再调用 Run。
func (r *Runner) Run() (*Result, error) {
	// 续跑：载入前一有效检查点，恢复已 ingest 到达数与规范事件集，
	// 续跑不重复计入包，且能完整重建中断前的时间线。
	if r.opt.Resume {
		if cp, err := r.store.Load(); err == nil {
			r.hadCheckpoint = true
			r.resumeFromArrival = int(cp.Seq) // Seq 存的是已处理到达数
			ingested, ierr := unmarshalIngested(cp.Payload)
			if ierr != nil {
				return nil, ierr
			}
			r.ingested = ingested
		} else if !media.IsKind(err, media.KindCheckpoint) {
			return nil, err
		}
	}
	r.stats.InputDigest = r.inputDigest()

	// 阶段 A：逐到达事件 ingest，支持中断点。
	interrupted, err := r.ingest()
	if err != nil {
		return nil, err
	}
	if interrupted {
		return &Result{Interrupted: true, Manifest: r.snapshotManifest()},
			media.E(media.KindTimeline, "pipeline.interrupt",
				media.ErrorLocation{ArrivalIndex: r.opt.Interrupt.AtArrival, ByteOffset: -1},
				"模拟中断触发，检查点已落盘，等待续跑", nil)
	}

	r.stats.Duplicates = uint64(r.dedup.Duplicates())
	r.stats.Conflicts = uint64(len(r.dedup.Conflicts()))

	// 时间线重建与占位填充。
	if err := r.buildTimeline(); err != nil {
		return nil, err
	}
	r.stats.Missing = uint64(r.replayer.MissingCount())

	// 阶段 B：渲染。
	if err := r.render(); err != nil {
		return nil, err
	}
	return r.finalize()
}

// ingest 处理到达事件；返回 interrupted=true 表示在计划点落检查点并暂停。
// 续跑时跳过检查点之前的到达事件（它们已计入上一段）。
func (r *Runner) ingest() (bool, error) {
	for arrival, e := range r.opt.Events {
		if arrival < r.resumeFromArrival {
			continue
		}
		r.bwe.Observe(e.ArrivalMicros)
		dec, err := r.dedup.Observe(e)
		if err != nil && !packet.IsConflict(err) {
			if media.IsKind(err, media.KindPacketCorrupt) {
				// 结构非法（CRC/越界等）：计入损坏，不进入时间线。
				r.stats.Corrupt++
				continue
			}
			return false, err
		}
		switch dec {
		case packet.DecisionDuplicate:
			continue
		case packet.DecisionKeptExisting:
			continue
		case packet.DecisionReplaced:
			// 胜者已在去重器，删除旧 ingested 条目，稍后统一重建。
			delete(r.ingested, keyToExt(e))
		}
		r.ingested[keyToExt(e)] = e

		if r.opt.Interrupt.Enabled && arrival == r.opt.Interrupt.AtArrival {
			if err := r.saveCheckpoint(arrival); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

// keyToExt 把事件序号转成临时扩展序号（ingest 阶段单流，直接用 uint16 值）。
// 真正的回绕展开在 buildTimeline 中按排序顺序统一完成。
func keyToExt(e *packet.Event) int64 { return int64(e.Seq) }

// buildTimeline 对已 ingest 事件做回绕展开并填充突发丢失占位。
func (r *Runner) buildTimeline() error {
	events := make([]*packet.Event, 0, len(r.ingested))
	for _, e := range r.ingested {
		events = append(events, e)
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].Epoch != events[j].Epoch {
			return events[i].Epoch < events[j].Epoch
		}
		return events[i].Seq < events[j].Seq
	})
	var prev int64
	have := false
	type ext struct {
		seq int64
		e   *packet.Event
	}
	ordered := make([]ext, 0, len(events))
	for _, e := range events {
		var es int64
		if !have {
			es = int64(e.Seq)
			have = true
		} else {
			es = packet.ExtendSeq(prev, e.Seq)
		}
		prev = es
		ordered = append(ordered, ext{es, e})
	}
	if len(ordered) == 0 {
		return nil
	}
	for _, x := range ordered {
		if _, err := r.replayer.Insert(x.seq, x.e); err != nil {
			return err
		}
	}
	// 填充 [min, max] 内所有缺口为占位（含突发丢失的中段区间），
	// 不能只用连续边界，否则第一个缺口之后的丢失永远无法填补。
	if len(ordered) == 0 {
		return nil
	}
	minSeq := ordered[0].seq
	maxSeq := ordered[len(ordered)-1].seq
	tmpl := *ordered[0].e
	for seq := minSeq; seq <= maxSeq; seq++ {
		if _, exists := r.replayer.Slot(seq); !exists {
			if _, err := r.replayer.FillMissing(seq, seq, tmpl); err != nil {
				return err
			}
		}
	}
	return nil
}

// render 逐槽位解码、重采样、整段响度归一并写 WAV。
func (r *Runner) render() error {
	accumulated := &audio.Buffer{}
	for _, slot := range r.replayer.SortedSlots() {
		var buf *audio.Buffer
		if slot.Missing {
			frames := int(slot.Event.DurationTicks) * audio.TargetRate /
				int(slot.Event.ClockRate)
			buf = audio.Silence(frames)
		} else {
			b, err := r.decodeOne(slot.Event)
			if err != nil {
				if media.IsKind(err, media.KindDecode) {
					r.stats.Corrupt++
					frames := int(slot.Event.DurationTicks) * audio.TargetRate /
						int(slot.Event.ClockRate)
					buf = audio.Silence(frames)
				} else {
					return err
				}
			} else {
				buf = b
			}
		}
		accumulated = audio.Append(accumulated, buf)
	}
	if r.resampler != nil {
		tail, err := r.resampler.Flush()
		if err != nil {
			return err
		}
		accumulated = audio.Append(accumulated, tail)
	}
	if accumulated.Frames > 0 {
		nres, err := audio.Normalize(accumulated)
		if err != nil {
			return err
		}
		r.debugNorm = nres
		r.debugAfter = audio.MeasureLoudness(accumulated)
	}
	w, err := NewWAVWriter(r.opt.OutputWAV, audio.TargetRate, 2, false, 0)
	if err != nil {
		return err
	}
	defer w.Close()
	if err := w.WriteBuffer(accumulated); err != nil {
		return err
	}
	if err := w.Finalize(); err != nil {
		return err
	}
	r.stats.OutputOffset = int64(wavHeaderLen + w.DataBytes())
	return nil
}

// decodeOne 延迟初始化解码器/重采样器。
func (r *Runner) decodeOne(e *packet.Event) (*audio.Buffer, error) {
	if r.decoder == nil {
		dec, err := packet.NewDecoder(e.Codec, r.opt.ExtraData)
		if err != nil {
			return nil, err
		}
		r.decoder = dec
		r.resampler = audio.NewResampler()
	}
	frame, err := r.decoder.Decode(e)
	if err != nil {
		return nil, err
	}
	if frame == nil {
		return &audio.Buffer{}, nil
	}
	return r.resampler.Convert(frame)
}

// saveCheckpoint 落盘 ingest 进度（输入摘要 + 已处理到达序 + 规范事件集）。
// 事件集写入 checkpoint.Payload，续跑据此完整重建时间线，不依赖重新接收前半段。
func (r *Runner) saveCheckpoint(arrival int) error {
	payload, err := r.marshalIngested()
	if err != nil {
		return err
	}
	cp := &journal.Checkpoint{
		Seq:             uint64(arrival) + 1,
		LastExtSeq:      int64(arrival),
		WatermarkMicros: r.opt.Events[arrival].ArrivalMicros,
		CountedPackets:  uint64(len(r.ingested)),
		InputDigest:     r.inputDigest(),
		Payload:         payload,
	}
	return r.store.Save(cp)
}

// marshalIngested 把已 ingest 事件按序号确定性序列化（长度前缀的记录串）。
func (r *Runner) marshalIngested() ([]byte, error) {
	seqs := make([]int64, 0, len(r.ingested))
	for s := range r.ingested {
		seqs = append(seqs, s)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	var out []byte
	var lenBuf [4]byte
	for _, s := range seqs {
		rec, err := r.ingested[s].MarshalBinary()
		if err != nil {
			return nil, media.Wrap(err, "pipeline.marshal-cp", "序列化检查点事件失败")
		}
		binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(rec)))
		out = append(out, lenBuf[:]...)
		out = append(out, rec...)
	}
	return out, nil
}

// unmarshalIngested 从检查点 Payload 还原规范事件集。
func unmarshalIngested(payload []byte) (map[int64]*packet.Event, error) {
	out := make(map[int64]*packet.Event)
	for off := 0; off < len(payload); {
		if off+4 > len(payload) {
			return nil, media.E(media.KindCheckpoint, "pipeline.unmarshal-cp",
				media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
				"检查点事件长度截断", nil)
		}
		n := int(binary.LittleEndian.Uint32(payload[off:]))
		off += 4
		if off+n > len(payload) {
			return nil, media.E(media.KindCheckpoint, "pipeline.unmarshal-cp",
				media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
				"检查点事件记录截断", nil)
		}
		e, err := packet.UnmarshalEvent(payload[off : off+n])
		if err != nil {
			return nil, media.Wrap(err, "pipeline.unmarshal-cp", "检查点事件损坏")
		}
		off += n
		out[int64(e.Seq)] = e
	}
	return out, nil
}

func (r *Runner) snapshotManifest() *journal.Manifest {
	m := r.stats
	m.Version = 1
	m.InputDigest = r.inputDigest()
	return &m
}

func (r *Runner) finalize() (*Result, error) {
	sum, err := sha256File(r.opt.OutputWAV)
	if err != nil {
		return nil, err
	}
	m := r.snapshotManifest()
	m.OutputFile = baseName(r.opt.OutputWAV)
	m.OutputDigest = sum
	m.Resume = r.opt.Resume
	if err := m.Write(journal.ManifestPath(r.opt.JournalDir)); err != nil {
		return nil, err
	}
	return &Result{
		Manifest:          m,
		OutputSHA256:      sum,
		DamagePacketDelta: int(r.stats.Corrupt),
	}, nil
}

// 引用 timeline 以保留水位（迟到检测在 ingest 中通过 bwe 间接使用）。
var _ = timeline.NewWatermark
