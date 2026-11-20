package repair

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"mediaroute/internal/index"
	"mediaroute/internal/packet"
	"mediaroute/internal/provenance"
	"mediaroute/internal/timeline"
)

// Config carries the parameters that affect output bytes.
type Config struct {
	Window   uint32 `json:"window"`
	DriftPPM uint32 `json:"drift_ppm"`
}

// ErrMediaBoundary marks media-boundary corruption (dependency
// cycles, missing dependencies); the CLI maps it to exit code 3.
var ErrMediaBoundary = errors.New("media boundary corruption")

// BuildGraph validates the session's track graph and returns tracks
// in dependency order.
func BuildGraph(sess *packet.Session) ([]*timeline.Track, error) {
	g := timeline.NewGraph()
	for _, td := range sess.Tracks {
		if err := g.Add(&timeline.Track{
			SSRC: td.SSRC, ClockRate: td.ClockRate, Kind: td.Kind, DependsOn: td.DependsOn,
		}); err != nil {
			return nil, err
		}
	}
	order, err := g.TopoOrder()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMediaBoundary, err)
	}
	return order, nil
}

// TrackMetas derives output track metadata in dependency order.
func TrackMetas(sess *packet.Session) ([]index.TrackMeta, error) {
	order, err := BuildGraph(sess)
	if err != nil {
		return nil, err
	}
	metas := make([]index.TrackMeta, len(order))
	for i, t := range order {
		metas[i] = index.TrackMeta{
			TrackID: t.TrackID, SSRC: t.SSRC,
			Timescale: t.ClockRate, MediaType: t.Kind,
		}
	}
	return metas, nil
}

// SHA256 returns the config digest used in checkpoints and manifests.
func (c Config) SHA256() string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fragTrack accumulates one track's samples within the open fragment.
type fragTrack struct {
	FirstExt, LastExt   uint64
	Count               int
	FirstNs, LastNs     int64
	FirstUnits          uint64
	LastUnits           uint64
	LastDelta           uint32
	Reordered, LossFlag bool
	Hash                []byte // chained sha256 over sample data
	pending             *index.SampleIn
}

// Pipeline drives records through ordering, timeline mapping,
// indexing and provenance collection.
type Pipeline struct {
	cfg   Config
	order []*timeline.Track
	bySSRC map[uint32]*timeline.Track
	re    *Reassembler
	ts    *TSRepair
	idx   *index.Writer
	man   *provenance.Manifest

	records    int
	rtpPackets int
	events     []Event
	tsErrors   []TSError
	firstDmg   uint64
	haveDmg    bool

	fragOpen bool
	frag     map[uint32]*fragTrack
	nonRAWhy  map[uint32]string
	nonRAStart map[uint32]int64
	pendLoss   map[uint32]bool
	pendReord  map[uint32]bool

	// OnFragment, if set, is called after each fragment commits;
	// used to checkpoint at complete media boundaries.
	OnFragment func()
}

// NewPipeline builds the track graph (rejecting cycles and missing
// dependencies) and prepares repair/index state.
func NewPipeline(cfg Config, sess *packet.Session, idxw *index.Writer) (*Pipeline, error) {
	order, err := BuildGraph(sess)
	if err != nil {
		return nil, err
	}
	ts := NewTSRepair(cfg.DriftPPM)
	bySSRC := map[uint32]*timeline.Track{}
	for _, t := range order {
		bySSRC[t.SSRC] = t
		ts.RegisterTrack(t.SSRC, t.ClockRate)
	}
	return &Pipeline{
		cfg: cfg, order: order, bySSRC: bySSRC,
		re: NewReassembler(cfg.Window), ts: ts, idx: idxw,
		man: provenance.NewManifest(),
		frag: map[uint32]*fragTrack{},
		nonRAWhy: map[uint32]string{}, nonRAStart: map[uint32]int64{},
		pendLoss: map[uint32]bool{}, pendReord: map[uint32]bool{},
	}, nil
}

// Manifest returns the accumulated provenance manifest.
func (p *Pipeline) Manifest() *provenance.Manifest { return p.man }

// Records returns the number of input records processed.
func (p *Pipeline) Records() int { return p.records }

// OutputOffset returns the committed output byte count.
func (p *Pipeline) OutputOffset() int64 { return p.idx.Offset() }

// TimeBases reports the time-base source of each track.
func (p *Pipeline) TimeBases() map[string]string {
	m := map[string]string{}
	for _, tr := range p.order {
		tb := "rtcp-sr"
		if p.ts.Estimated(tr.SSRC) {
			tb = "estimated"
		}
		m[strconv.FormatUint(uint64(tr.SSRC), 10)] = tb
	}
	return m
}

// Process handles one input record.
func (p *Pipeline) Process(rec *packet.Record) error {
	p.records++
	switch {
	case rec.Truncated:
		p.man.Errors = append(p.man.Errors, provenance.ErrorEntry{
			Kind: "truncated-record", Detail: fmt.Sprintf("record %d: header damaged", rec.Index)})
	case rec.SR != nil:
		if _, ok := p.bySSRC[rec.SR.SSRC]; !ok {
			return fmt.Errorf("record %d: Sender Report for unknown SSRC %d", rec.Index, rec.SR.SSRC)
		}
		if e := p.ts.AddSenderReport(rec.SR); e != nil {
			p.addTSError(*e)
		}
	case rec.RTP != nil:
		if _, ok := p.bySSRC[rec.RTP.SSRC]; !ok {
			return fmt.Errorf("record %d: RTP for unknown SSRC %d", rec.Index, rec.RTP.SSRC)
		}
		p.rtpPackets++
		emitted, events := p.re.Push(rec.RTP.SSRC, rec.RTP, rec.Index)
		for _, ev := range events {
			p.handleOrderEvent(ev)
		}
		for _, em := range emitted {
			if err := p.handleSample(em); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Pipeline) handleOrderEvent(ev Event) {
	p.events = append(p.events, ev)
	p.man.Errors = append(p.man.Errors, provenance.ErrorEntry{
		Kind: ev.Kind.String(), SSRC: ev.SSRC, ExtSeq: ev.ExtSeq, Detail: ev.Detail})
	switch ev.Kind {
	case EvLoss, EvConflict, EvLate, EvReorder:
		p.noteDamage(ev.ExtSeq)
	}
	tr := p.bySSRC[ev.SSRC]
	mark := func(set func(ft *fragTrack)) {
		if ft := p.frag[ev.SSRC]; ft != nil {
			set(ft)
		}
	}
	switch ev.Kind {
	case EvLoss:
		mark(func(ft *fragTrack) { ft.LossFlag = true })
		p.pendLoss[ev.SSRC] = true
		if tr != nil && tr.Kind == "video" {
			p.nonRAWhy[ev.SSRC] = "loss"
		}
	case EvReorder:
		mark(func(ft *fragTrack) { ft.Reordered = true })
		p.pendReord[ev.SSRC] = true
	}
}

func (p *Pipeline) noteDamage(extSeq uint64) {
	if !p.haveDmg || extSeq < p.firstDmg {
		p.firstDmg = extSeq
		p.haveDmg = true
	}
}

func (p *Pipeline) addTSError(e TSError) {
	p.tsErrors = append(p.tsErrors, e)
	p.man.Errors = append(p.man.Errors, provenance.ErrorEntry{
		Kind: e.Kind, SSRC: e.SSRC, ExtSeq: e.ExtSeq,
		DeviationNs: e.DeviationNs, Detail: e.Detail})
	p.noteDamage(e.ExtSeq)
	if e.Kind == "backward" {
		if tr := p.bySSRC[e.SSRC]; tr != nil && tr.Kind == "video" {
			p.nonRAWhy[e.SSRC] = "timestamp-out-of-range"
		}
	}
}

func (p *Pipeline) handleSample(em Emitted) error {
	tr := p.bySSRC[em.Packet.SSRC]
	ns, units, tsErr := p.ts.Map(em.Packet.SSRC, em.ExtSeq, em.Packet.Timestamp)
	if tsErr != nil {
		p.addTSError(*tsErr)
	}
	payload := em.Packet.Payload
	isVideo := tr.Kind == "video"
	sync := isVideo && len(payload) > 0 && payload[0] == 0x65
	if isVideo && len(payload) < 16 {
		p.man.Errors = append(p.man.Errors, provenance.ErrorEntry{
			Kind: "truncated-nal", SSRC: tr.SSRC, ExtSeq: em.ExtSeq,
			Detail: "NAL unit truncated; sample not decodable"})
		p.noteDamage(em.ExtSeq)
		p.nonRAWhy[tr.SSRC] = "truncated-nal"
	}
	if _, ok := p.nonRAWhy[tr.SSRC]; ok {
		if _, has := p.nonRAStart[tr.SSRC]; !has {
			p.nonRAStart[tr.SSRC] = ns // first sample at/after the fault
		}
	}
	if sync {
		if p.fragOpen && p.frag[tr.SSRC] != nil && p.frag[tr.SSRC].Count > 0 {
			if err := p.flushFragment(); err != nil {
				return err
			}
		}
		p.closeNonRA(tr, ns)
	}
	if !p.fragOpen {
		p.fragOpen = true
	}
	ft := p.frag[tr.SSRC]
	if ft == nil {
		ft = &fragTrack{FirstExt: em.ExtSeq, FirstNs: ns, FirstUnits: uint64(units)}
		if p.pendLoss[tr.SSRC] {
			ft.LossFlag = true
			delete(p.pendLoss, tr.SSRC)
		}
		if p.pendReord[tr.SSRC] {
			ft.Reordered = true
			delete(p.pendReord, tr.SSRC)
		}
		p.frag[tr.SSRC] = ft
	}
	// Duration of the previous sample becomes known now.
	if ft.pending != nil {
		ft.pending.Dur = uint32(units - int64(ft.pending.DecodeTime))
		ft.LastDelta = ft.pending.Dur
		p.idx.AddSample(*ft.pending)
	}
	ft.pending = &index.SampleIn{
		TrackID: tr.TrackID, Data: payload, DecodeTime: uint64(units), Sync: sync,
	}
	ft.LastExt, ft.LastNs = em.ExtSeq, ns
	ft.LastUnits = uint64(units)
	ft.Count++
	sum := sha256.Sum256(append(ft.Hash, payload...))
	ft.Hash = sum[:]
	return nil
}

// closeNonRA closes a pending non-random-access interval at a keyframe.
func (p *Pipeline) closeNonRA(tr *timeline.Track, keyNs int64) {
	why, ok := p.nonRAWhy[tr.SSRC]
	if !ok {
		return
	}
	delete(p.nonRAWhy, tr.SSRC)
	start := p.nonRAStart[tr.SSRC]
	delete(p.nonRAStart, tr.SSRC)
	if keyNs > start {
		p.man.NonRandomAccess = append(p.man.NonRandomAccess, provenance.Interval{
			TrackID: tr.TrackID, SSRC: tr.SSRC, FromNs: start, ToNs: keyNs, Reason: why})
	}
}

func (p *Pipeline) flushFragment() error {
	// Finalize pending samples with the last observed delta.
	for _, tr := range p.order {
		ft := p.frag[tr.SSRC]
		if ft != nil && ft.pending != nil {
			ft.pending.Dur = ft.LastDelta
			p.idx.AddSample(*ft.pending)
			ft.pending = nil
		}
	}
	if _, _, err := p.idx.FlushFragment(); err != nil {
		return err
	}
	fragSeq := p.idx.FragSeq()
	for _, tr := range p.order {
		ft := p.frag[tr.SSRC]
		if ft == nil || ft.Count == 0 {
			continue
		}
		kind := "sample-boundary"
		if tr.Kind == "video" {
			kind = "keyframe"
		}
		repair := "none"
		if ft.LossFlag {
			repair = "loss-gap-before"
		} else if ft.Reordered {
			repair = "reordered"
		}
		timeBase := "rtcp-sr"
		if p.ts.Estimated(tr.SSRC) {
			timeBase = "estimated"
		}
		// Duration comes from corrected monotonic units, so a
		// re-anchor inside the fragment cannot distort it.
		durNs := int64(ft.LastUnits+uint64(ft.LastDelta)-ft.FirstUnits) *
			1_000_000_000 / int64(tr.ClockRate)
		p.man.Entries = append(p.man.Entries, provenance.Entry{
			Index: len(p.man.Entries), Fragment: fragSeq,
			TrackID: tr.TrackID, SSRC: tr.SSRC, Kind: kind,
			DecodeTimeNs: ft.FirstNs, DecodeTimeUnit: ft.FirstUnits,
			Timescale: tr.ClockRate, DurationNs: durNs,
			SrcFirstExtSeq: ft.FirstExt, SrcLastExtSeq: ft.LastExt,
			Repair: repair, TimeBase: timeBase,
			SHA256: hex.EncodeToString(ft.Hash),
		})
	}
	p.frag = map[uint32]*fragTrack{}
	if p.OnFragment != nil {
		p.OnFragment()
	}
	return nil
}

// Finish drains the reassembler and closes the final fragment.
func (p *Pipeline) Finish() error {
	for _, tr := range p.order {
		emitted, events := p.re.Flush(tr.SSRC)
		for _, ev := range events {
			p.handleOrderEvent(ev)
		}
		for _, em := range emitted {
			if err := p.handleSample(em); err != nil {
				return err
			}
		}
	}
	if p.idx.Pending() > 0 || p.pendingSamples() {
		return p.flushFragment()
	}
	return nil
}

func (p *Pipeline) pendingSamples() bool {
	for _, ft := range p.frag {
		if ft != nil && ft.pending != nil {
			return true
		}
	}
	return false
}

// Report summarizes a completed (or interrupted) run.
type Report struct {
	Records         int
	RTPPackets      int
	Tracks          int
	Events          []Event
	TSErrors        []TSError
	FirstDamageSeq  uint64
	HasDamage       bool
	IndexEntries    int
}

// Report returns the current pipeline report.
func (p *Pipeline) Report() Report {
	return Report{
		Records: p.records, RTPPackets: p.rtpPackets, Tracks: len(p.order),
		Events: p.events, TSErrors: p.tsErrors,
		FirstDamageSeq: p.firstDmg, HasDamage: p.haveDmg,
		IndexEntries: len(p.man.Entries),
	}
}

// State is the serializable pipeline state for checkpointing.
type State struct {
	Reassembler map[uint32]TrackState
	Clocks      map[uint32]ClockState
	Records     int
	RTPPackets  int
	Events      []Event
	TSErrors    []TSError
	FirstDmg    uint64
	HaveDmg     bool
	Manifest    *provenance.Manifest
	FragOpen    bool
	Frag        map[uint32]*fragTrackState
	NonRAWhy    map[uint32]string
	NonRAStart  map[uint32]int64
	PendLoss    map[uint32]bool
	PendReord   map[uint32]bool
	Index       *index.State
}

type fragTrackState struct {
	FirstExt, LastExt   uint64
	Count               int
	FirstNs, LastNs     int64
	FirstUnits          uint64
	LastUnits           uint64
	LastDelta           uint32
	Reordered, LossFlag bool
	Hash                []byte
	Pending             *index.PendingSample
}

// Export serializes the full pipeline state.
func (p *Pipeline) Export() (*State, error) {
	st := &State{
		Reassembler: p.re.State(), Clocks: p.ts.State(),
		Records: p.records, RTPPackets: p.rtpPackets,
		Events: p.events, TSErrors: p.tsErrors,
		FirstDmg: p.firstDmg, HaveDmg: p.haveDmg,
		Manifest: p.man, FragOpen: p.fragOpen,
		NonRAWhy: p.nonRAWhy, NonRAStart: p.nonRAStart,
		PendLoss: p.pendLoss, PendReord: p.pendReord,
		Index: p.idx.State(),
		Frag:  map[uint32]*fragTrackState{},
	}
	for ssrc, ft := range p.frag {
		fs := &fragTrackState{
			FirstExt: ft.FirstExt, LastExt: ft.LastExt, Count: ft.Count,
			FirstNs: ft.FirstNs, LastNs: ft.LastNs, FirstUnits: ft.FirstUnits,
			LastUnits: ft.LastUnits,
			LastDelta: ft.LastDelta, Reordered: ft.Reordered, LossFlag: ft.LossFlag,
			Hash: ft.Hash,
		}
		if ft.pending != nil {
			fs.Pending = &index.PendingSample{
				Data: ft.pending.Data, DecodeTime: ft.pending.DecodeTime,
				Dur: ft.pending.Dur, Sync: ft.pending.Sync,
			}
		}
		st.Frag[ssrc] = fs
	}
	return st, nil
}

// Import restores a previously exported state. The index writer must
// already wrap the truncated output file.
func (p *Pipeline) Import(st *State, committedOffset int64) error {
	if err := p.re.Restore(st.Reassembler); err != nil {
		return err
	}
	p.ts.Restore(st.Clocks)
	p.records, p.rtpPackets = st.Records, st.RTPPackets
	p.events, p.tsErrors = st.Events, st.TSErrors
	p.firstDmg, p.haveDmg = st.FirstDmg, st.HaveDmg
	p.man = st.Manifest
	p.fragOpen = st.FragOpen
	p.nonRAWhy, p.nonRAStart = st.NonRAWhy, st.NonRAStart
	p.pendLoss, p.pendReord = st.PendLoss, st.PendReord
	if p.nonRAWhy == nil {
		p.nonRAWhy = map[uint32]string{}
	}
	if p.nonRAStart == nil {
		p.nonRAStart = map[uint32]int64{}
	}
	if p.pendLoss == nil {
		p.pendLoss = map[uint32]bool{}
	}
	if p.pendReord == nil {
		p.pendReord = map[uint32]bool{}
	}
	p.frag = map[uint32]*fragTrack{}
	for ssrc, fs := range st.Frag {
		ft := &fragTrack{
			FirstExt: fs.FirstExt, LastExt: fs.LastExt, Count: fs.Count,
			FirstNs: fs.FirstNs, LastNs: fs.LastNs, FirstUnits: fs.FirstUnits,
			LastUnits: fs.LastUnits,
			LastDelta: fs.LastDelta, Reordered: fs.Reordered, LossFlag: fs.LossFlag,
			Hash: fs.Hash,
		}
		if fs.Pending != nil {
			ft.pending = &index.SampleIn{
				TrackID: p.bySSRC[ssrc].TrackID, Data: fs.Pending.Data,
				DecodeTime: fs.Pending.DecodeTime, Dur: fs.Pending.Dur, Sync: fs.Pending.Sync,
			}
		}
		p.frag[ssrc] = ft
	}
	p.idx.Restore(st.Index, committedOffset)
	return nil
}
