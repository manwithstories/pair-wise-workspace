// Package app wires the modules into the deterministic archive
// pipeline: RTP/RTCP parse and reorder -> shared clock model ->
// timeline watermark and replay -> audio/video processing -> segmented
// archive with verification.
package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"

	"archiver/internal/archive"
	"archiver/internal/audio"
	"archiver/internal/clock"
	"archiver/internal/fixture"
	"archiver/internal/media"
	"archiver/internal/rtp"
	"archiver/internal/timeline"
	"archiver/internal/video"
)

const (
	latenessUs = 500_000
	horizonUs  = 2_000_000
	sliceUs    = 2_000_000
)

// Config is one archiver invocation.
type Config struct {
	Case           string
	Seed           uint64
	Inject         []string
	Out            string
	InterruptAfter int
	Resume         bool
}

// InputError marks unrecoverable input or parameter problems (exit 2).
type InputError struct{ Reason string }

func (e *InputError) Error() string { return e.Reason }

// Metrics carries every printed acceptance value.
type Metrics struct {
	Parsed           int
	RoundTrip        float64
	FirstErrorOffset int64
	Lost             int
	Duplicates       int
	Reordered        int
	OutOfWindow      int
	Replays          int
	Quarantined      int
	Segments         int
	SegmentHashMatch float64
	KeyframeHit      float64
	SampleRate       int
	Layout           string
	IntegratedLUFS   float64
	TruePeakDBTP     float64
	ReusedSegments   int
	FreshHashMatch   float64
	Interrupted      bool
}

// muxSink fans committed events out to the segmenter and a retained
// log (for audio/video processing), rolling both back together.
type muxSink struct {
	seg    *archive.Segmenter
	events []timeline.Event
}

func (m *muxSink) Emit(ev timeline.Event) {
	m.seg.Emit(ev)
	m.events = append(m.events, ev)
}

func (m *muxSink) Snapshot() []byte {
	b, _ := json.Marshal(struct {
		Seg json.RawMessage
		N   int
	}{m.seg.Snapshot(), len(m.events)})
	return b
}

func (m *muxSink) Restore(snap []byte) {
	var s struct {
		Seg json.RawMessage
		N   int
	}
	if err := json.Unmarshal(snap, &s); err != nil {
		panic(err)
	}
	m.seg.Restore(s.Seg)
	m.events = m.events[:s.N]
}

func (m *muxSink) OnWatermark(wm media.TimeUs) { m.seg.OnWatermark(wm) }

// Run executes the pipeline and returns the acceptance metrics.
func Run(cfg Config) (*Metrics, error) {
	stream := fixture.Generate(fixture.Options{Seed: cfg.Seed, Inject: cfg.Inject})
	sum := sha256.Sum256(stream)
	digest := hex.EncodeToString(sum[:])
	params := archive.Params{
		Case: cfg.Case, Seed: cfg.Seed, Inject: cfg.Inject,
		LatenessUs: latenessUs, HorizonUs: horizonUs, SliceUs: sliceUs,
	}
	if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
		return nil, err
	}
	var reuse map[int64]string
	if cfg.Resume {
		cp, err := archive.LoadCheckpoint(cfg.Out)
		if err != nil {
			return nil, &InputError{Reason: fmt.Sprintf("no usable checkpoint: %v", err)}
		}
		if reason := archive.CheckResume(cp, digest, params); reason != "" {
			return nil, &InputError{Reason: "resume refused: " + reason}
		}
		reuse = cp.SegmentHashes
	}

	m := &Metrics{FirstErrorOffset: -1, SampleRate: audio.OutRate, Layout: "stereo"}
	tracks := fixture.Tracks()
	models := map[uint32]*clock.Model{}
	for ssrc, spec := range tracks {
		models[ssrc] = clock.NewModel(spec.Rate)
	}

	// Pass 1: parse, reorder, map to media time.
	rd := rtp.NewReader(bytes.NewReader(stream), 0)
	reorder := rtp.NewReorderer(128)
	var events []timeline.Event
	rtOK := 0
	emit := func(p *rtp.Packet) {
		spec := tracks[p.SSRC]
		us, _ := models[p.SSRC].ToMediaTime(p.Timestamp)
		events = append(events, timeline.Event{
			Track:  media.TrackID{Kind: spec.Kind, SSRC: p.SSRC},
			TimeUs: us, ExtSeq: p.ExtSeq, Data: p.Payload,
		})
	}
	for {
		p, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			var pe *media.PacketError
			if !errors.As(err, &pe) || pe.Fatal {
				return nil, &InputError{Reason: fmt.Sprintf("unrecoverable input: %v", err)}
			}
			if m.FirstErrorOffset < 0 {
				m.FirstErrorOffset = pe.Offset
			}
			continue
		}
		m.Parsed++
		if bytes.Equal(p.Serialize(), p.Raw) {
			rtOK++
		}
		if p.RTCP {
			if _, ok := models[p.SR.SSRC]; ok {
				models[p.SR.SSRC].AddSenderReport(p.SR.NTPSec, p.SR.NTPFrac, p.SR.RTPTimestamp)
			}
			continue
		}
		emitted, oow := reorder.Push(p)
		if oow != nil {
			m.OutOfWindow++
		}
		for _, p := range emitted {
			emit(p)
		}
	}
	for _, p := range reorder.Flush() {
		emit(p)
	}
	st := reorder.Stats()
	m.Lost, m.Duplicates, m.Reordered = st.Lost, st.Duplicates, st.Reordered
	m.RoundTrip = pct(rtOK, m.Parsed)

	// Normalize to a zero-based run timeline.
	var base media.TimeUs
	for i, ev := range events {
		if i == 0 || ev.TimeUs < base {
			base = ev.TimeUs
		}
	}
	for i := range events {
		events[i].TimeUs -= base
	}

	// Pass 2: watermark, replay, segmented archive.
	seg := archive.NewSegmenter(cfg.Out, sliceUs, horizonUs, reuse)
	sink := &muxSink{seg: seg}
	clockSnap := func() []byte {
		snaps := map[uint32]clock.Snapshot{}
		for ssrc, md := range models {
			snaps[ssrc] = md.Snapshot()
		}
		b, _ := json.Marshal(snaps)
		return b
	}
	clockLoad := func(b []byte) {
		var snaps map[uint32]clock.Snapshot
		if err := json.Unmarshal(b, &snaps); err != nil {
			panic(err)
		}
		for ssrc, sn := range snaps {
			models[ssrc] = clock.RestoreModel(sn)
		}
	}
	wm := timeline.NewWatermark(latenessUs)
	rep := timeline.NewReplayer(wm, horizonUs, 256, sink, clockSnap, clockLoad)
	for _, ev := range events {
		rep.Add(ev)
		if cfg.InterruptAfter > 0 && seg.Committed() >= cfg.InterruptAfter {
			cp := archive.Checkpoint{
				InputDigest: digest, ParamsHash: params.Hash(),
				SegmentsCommitted: seg.Committed(), WatermarkUs: int64(rep.Drained()),
				Clock: map[uint32]clock.Snapshot{}, SegmentHashes: map[int64]string{},
			}
			json.Unmarshal(clockSnap(), &cp.Clock)
			for _, s := range seg.Segments() {
				cp.SegmentHashes[s.Slice] = s.SHA256
			}
			if err := archive.SaveCheckpoint(cfg.Out, cp); err != nil {
				return nil, err
			}
			m.Interrupted = true
			m.Segments = seg.Committed()
			return m, nil
		}
	}
	rep.Flush()
	seg.Flush()
	m.Replays, m.Quarantined = rep.Stats()
	m.Quarantined += m.OutOfWindow
	m.Segments = seg.Committed()

	if err := finishAudio(m, sink.events); err != nil {
		return nil, err
	}
	finishVideo(m, cfg, sink.events)

	if err := seg.WriteManifest(digest, params); err != nil {
		return nil, err
	}
	matched, total, firstErr, err := archive.Verify(cfg.Out)
	if err != nil {
		return nil, err
	}
	if firstErr != nil {
		return nil, &InputError{Reason: "verify: " + firstErr.Error()}
	}
	m.SegmentHashMatch = pct(matched, total)
	m.ReusedSegments = seg.Reused
	m.FreshHashMatch = pct(seg.Reused-seg.ReuseMismatch, seg.Reused)
	return m, nil
}

func pct(ok, total int) float64 {
	if total == 0 {
		return 100
	}
	return 100 * float64(ok) / float64(total)
}

// finishAudio rebuilds the 44.1 kHz mono signal from committed audio
// events, derives missing intervals from sequence gaps and normalizes.
func finishAudio(m *Metrics, events []timeline.Event) error {
	var audioEv []timeline.Event
	for _, ev := range events {
		if ev.Track.Kind == media.TrackAudio {
			audioEv = append(audioEv, ev)
		}
	}
	if len(audioEv) == 0 {
		return &InputError{Reason: "no audio events committed"}
	}
	base := audioEv[0].ExtSeq
	maxExt := base
	for _, ev := range audioEv {
		if ev.ExtSeq > maxExt {
			maxExt = ev.ExtSeq
		}
	}
	n := int(maxExt-base+1) * fixture.AudioSamplesPerPacket
	mono := make([]float32, n)
	present := make([]bool, int(maxExt-base+1))
	for _, ev := range audioEv {
		idx := int(ev.ExtSeq - base)
		if present[idx] {
			continue
		}
		present[idx] = true
		off := idx * fixture.AudioSamplesPerPacket
		for i := 0; i+1 < len(ev.Data) && i/2 < fixture.AudioSamplesPerPacket; i += 2 {
			mono[off+i/2] = float32(int16(binary.BigEndian.Uint16(ev.Data[i:]))) / 32767
		}
	}
	var missing [][2]int
	for i, ok := range present {
		if !ok {
			s := i * fixture.AudioSamplesPerPacket
			if len(missing) > 0 && missing[len(missing)-1][1] == s {
				missing[len(missing)-1][1] = s + fixture.AudioSamplesPerPacket
			} else {
				missing = append(missing, [2]int{s, s + fixture.AudioSamplesPerPacket})
			}
		}
	}
	res := audio.Normalize(mono, missing)
	m.IntegratedLUFS = res.IntegratedLUFS
	m.TruePeakDBTP = res.TruePeakDBTP
	return nil
}

// finishVideo builds the keyframe index over committed video events
// and measures random-access seek accuracy.
func finishVideo(m *Metrics, cfg Config, events []timeline.Event) {
	var stream []byte
	for _, ev := range events {
		if ev.Track.Kind == media.TrackVideo {
			stream = append(stream, ev.Data...)
		}
	}
	frames, err := video.ParseStream(stream)
	if err != nil || len(frames) == 0 {
		m.KeyframeHit = 0
		return
	}
	ix := video.Build(frames)
	rng := rand.New(rand.NewPCG(cfg.Seed, 99))
	targets := ix.Keyframes()
	for i := 0; i < 20; i++ {
		targets = append(targets, frames[rng.IntN(len(frames))].PTS)
	}
	hits := 0
	for _, t := range targets {
		if hit, _ := ix.Seek(t); hit {
			hits++
		}
	}
	m.KeyframeHit = pct(hits, len(targets))
}
