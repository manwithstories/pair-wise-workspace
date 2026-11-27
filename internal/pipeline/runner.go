// Package pipeline is the bounded orchestrator. It parses fragment groups,
// feeds RTP windows, builds explicit timelines and recovery plans in track
// dependency order, then aligns, indexes and audits the output. Every stage
// carries an explicit upper bound so a corrupt or lossy stream cannot make the
// process allocate or wait without limit.
package pipeline

import (
	"fmt"
	stdsync "sync"

	"pairwise/ingestcheck/internal/audit"
	"pairwise/ingestcheck/internal/clock"
	"pairwise/ingestcheck/internal/index"
	"pairwise/ingestcheck/internal/mp4"
	"pairwise/ingestcheck/internal/recovery"
	rtpadapt "pairwise/ingestcheck/internal/rtp"
	syncmod "pairwise/ingestcheck/internal/sync"
	"pairwise/ingestcheck/internal/track"
)

// FragmentInput is one fragment group entry: the moof+mdat bytes plus the RTP
// datagrams that delivered it. Packets may be sparse to model burst loss,
// Missing counts datagrams that never arrived, and Bandwidth is the measured
// link rate for the window that delivered this fragment.
type FragmentInput struct {
	TrackID   uint32
	Seq       uint32
	Data      []byte
	Packets   [][]byte
	Missing   int
	Bandwidth float64
}

// Config bundles every stage's bound.
type Config struct {
	MP4        mp4.ParseConfig
	RTP        rtpadapt.Config
	Recovery   recovery.Config
	Sync       syncmod.Config
	MaxFrags   int
	MaxKeys    int
	MaxDigests int
	Workers    int
}

// DefaultConfig returns deterministic, bounded settings.
func DefaultConfig() Config {
	return Config{
		MP4:        mp4.DefaultConfig(),
		RTP:        rtpadapt.DefaultConfig(),
		Recovery:   recovery.DefaultConfig(),
		Sync:       syncmod.DefaultConfig(),
		MaxFrags:   256,
		MaxKeys:    4096,
		MaxDigests: 256,
		Workers:    2,
	}
}

// Isolation records a fragment the parser rejected, with the first bad byte
// so a media engineer can locate the damage.
type Isolation struct {
	TrackID uint32
	Seq     uint32
	Offset  int64
	Code    clock.Code
	Detail  string
}

// FragmentHash is one hashed fragment.
type FragmentHash struct {
	TrackID uint32
	Seq     uint32
	Bytes   int
	Sha256  string
}

// TrackReport captures per-track accounting.
type TrackReport struct {
	TrackID     uint32
	Parsed      int
	Isolated    int
	Epochs      int
	HeaderGaps  int
	PacketGaps  int
	Recovered   int
	Explicit    int
	Duplicate   int
	Late        int
	Lost        int
	MaxHeldPeak int
	Keyframes   int
}

// Report is the complete, deterministic outcome of one run.
type Report struct {
	Tracks           []TrackReport
	Isolated         int
	IsolationCodes   []string
	FirstBadByte     int64
	RoundTripOK      int
	RoundTripTotal   int
	SegmentHashes    []FragmentHash
	SegmentSHATotal  int
	SegmentSHAOK     int
	DuplicateDrop    int
	Recovered        int
	ExplicitGap      int
	SkewP95Ms        float64
	SkewMaxMs        float64
	Unsynchronized   int
	Evaluated        int
	KeyframeHits     int
	KeyframeTotal    int
	IndexFingerprint uint64
	AuditRoot        string
	Errors           []string
}

// Group is the input to a run.
type Group struct {
	Graph     *track.Graph
	Fragments []FragmentInput
}

// Runner executes one group. It serialises concurrent runs on one instance.
type Runner struct {
	cfg  Config
	mu   stdsync.Mutex
	runs int
}

// New validates the config bounds and returns a runner.
func New(cfg Config) (*Runner, error) {
	if cfg.MaxFrags <= 0 || cfg.MaxKeys <= 0 || cfg.MaxDigests <= 0 || cfg.Workers <= 0 {
		return nil, clock.New(clock.CodeArgument, "pipeline", "non-positive pipeline bound")
	}
	if cfg.Workers > 32 {
		cfg.Workers = 32
	}
	return &Runner{cfg: cfg}, nil
}

// mediaTick is one fragment start in a track's native time base.
type mediaTick struct {
	tick int64
	rate clock.Rate
}

// trackOut carries the per-track results collected during the run.
type trackOut struct {
	report TrackReport
	frags  []*mp4.Fragment
	starts []mediaTick
}

// trackState is the rolling decode-time and sequence state for a track.
type trackState struct {
	prevBase uint64
	haveBase bool
	prevSeq  uint32
	haveSeq  bool
	wall     int64
	timeline clock.Timeline
}

// Run processes the group and returns a deterministic report. Corrupt
// fragments are isolated and the run continues; a structural error (cycle,
// missing track) aborts with a shared-model error.
func (r *Runner) Run(g Group) (*Report, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs++

	if g.Graph == nil {
		return nil, clock.New(clock.CodeTrackMissing, "pipeline", "nil track graph")
	}
	order := g.Graph.Order()
	if len(order) == 0 {
		return nil, clock.New(clock.CodeTrackMissing, "pipeline", "empty track graph")
	}
	if len(g.Fragments) > r.cfg.MaxFrags {
		return nil, clock.New(clock.CodeArgument, "pipeline", "fragment count exceeds bound")
	}

	byTrack := map[uint32][]FragmentInput{}
	for _, f := range g.Fragments {
		if _, ok := g.Graph.Node(f.TrackID); !ok {
			return nil, clock.New(clock.CodeTrackMissing, "pipeline", fmt.Sprintf("fragment for unknown track %d", f.TrackID))
		}
		byTrack[f.TrackID] = append(byTrack[f.TrackID], f)
	}

	hasher, err := audit.New(r.cfg.MaxDigests)
	if err != nil {
		return nil, err
	}
	aligner, err := syncmod.New(r.cfg.Sync)
	if err != nil {
		return nil, err
	}
	meter, err := recovery.NewMeter(r.cfg.Recovery)
	if err != nil {
		return nil, err
	}

	rep := &Report{FirstBadByte: -1}
	outputs := make(map[uint32]*trackOut, len(order))

	for _, id := range order {
		if perr := r.processTrack(id, g, byTrack[id], outputs, rep, hasher, meter); perr != nil {
			return nil, perr
		}
	}

	// Build the keyframe index per track. The hit metric probes every indexed
	// entry of the video track at its own decode time.
	ixs := make(map[uint32]*index.Index, len(order))
	for _, id := range order {
		ix, ierr := index.Build(id, outputs[id].frags, r.cfg.MaxKeys)
		if ierr != nil {
			return nil, ierr
		}
		ixs[id] = ix
		rep.IndexFingerprint ^= ix.Fingerprint()
		for i := range rep.Tracks {
			if rep.Tracks[i].TrackID == id {
				rep.Tracks[i].Keyframes = ix.Len()
			}
		}
	}
	for _, id := range order {
		if g.Graph.KindOf(id) != track.Video {
			continue
		}
		for _, e := range ixs[id].Entries() {
			rep.KeyframeTotal++
			if _, ok := ixs[id].Lookup(id, e.Time); ok {
				rep.KeyframeHits++
			}
		}
	}

	// Align audio to video at each shared fragment ordinal. Both sides are
	// compared in microseconds via the shared clock model; a pair that cannot
	// be formed is marked explicit rather than fabricated.
	v := videoTrack(g.Graph)
	for _, id := range order {
		if g.Graph.KindOf(id) != track.Audio {
			continue
		}
		if v == 0 {
			aligner.MarkExplicit()
			continue
		}
		vStarts := outputs[v].starts
		aStarts := outputs[id].starts
		n := len(vStarts)
		if len(aStarts) < n {
			n = len(aStarts)
		}
		if n == 0 {
			aligner.MarkExplicit()
			continue
		}
		for i := 0; i < n; i++ {
			if eerr := aligner.Evaluate(syncmod.Pair{
				Index:    i,
				Video:    vStarts[i].tick,
				VideoRt:  vStarts[i].rate,
				Audio:    aStarts[i].tick,
				AudioRt:  aStarts[i].rate,
				Fragment: uint32(i),
				Track:    id,
			}); eerr != nil {
				return nil, eerr
			}
		}
	}
	res := aligner.Finish()
	rep.Evaluated = res.Evaluated
	rep.SkewP95Ms = res.P95Ms
	rep.SkewMaxMs = res.MaxMs
	rep.Unsynchronized = res.Unsynchronized

	sum := hasher.Summary()
	rep.SegmentHashes = make([]FragmentHash, len(sum.Fragments))
	for i, d := range sum.Fragments {
		rep.SegmentHashes[i] = FragmentHash{TrackID: d.Track, Seq: d.Sequence, Bytes: d.Bytes, Sha256: d.Sha256}
	}
	rep.AuditRoot = sum.Root
	return rep, nil
}
