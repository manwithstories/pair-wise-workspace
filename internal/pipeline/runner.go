package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"

	"avpipe/internal/abr"
	"avpipe/internal/index"
	"avpipe/internal/ingest"
	"avpipe/internal/media"
	"avpipe/internal/segment"
	"avpipe/internal/transport"
)

// Config is the validated run configuration shared by CLI and tests.
type Config struct {
	OutDir         string
	InputPath      string
	FrameCount     uint32
	FPS            uint32
	SegmentMS      uint32
	Targets        []int
	Seed           int64
	InterruptAfter int // commit this many groups, then simulate a crash
	Resume         bool
}

// ErrInterrupted marks the simulated mid-run crash after committed groups.
var ErrInterrupted = errors.New("simulated interrupt after committed groups")

// Runner executes one pipeline pass over an already-generated container.
type Runner struct {
	cfg     Config
	journal *index.Journal
	state   *index.State
	report  *Report
	rng     *rand.Rand
}

// Run executes the pipeline and returns the final report. When the
// configured interrupt point is reached it returns ErrInterrupted together
// with a partial report; the caller re-invokes Run with Resume=true.
func Run(cfg Config) (*Report, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	r := &Runner{cfg: cfg, rng: rand.New(rand.NewSource(cfg.Seed + 99)), report: NewReport(cfg)}
	inputHash, err := hashFile(cfg.InputPath)
	if err != nil {
		return nil, media.NewError(media.ClassValidation, "pipeline", "input hash failed: "+err.Error(), 0, false)
	}
	cp := index.Checkpoint{
		InputHash:  inputHash,
		ConfigHash: index.ConfigDigest(cfg.Targets, cfg.FPS, cfg.SegmentMS),
		TargetKbps: cfg.Targets,
		FPS:        cfg.FPS,
		SegmentMS:  cfg.SegmentMS,
	}
	if err := r.openState(cp); err != nil {
		return nil, err
	}
	defer r.journal.Close()

	packets, corrupt, err := r.readPackets()
	if err != nil {
		return nil, err
	}
	r.report.CorruptPackets = corrupt
	ordered, dedup, dedupErrs := transport.ReorderDedup(packets)
	r.report.Dedup = dedup
	r.report.appendErrors(dedupErrs)
	recovered, fecStats, fecErrs := transport.DecodeFEC(ordered)
	r.report.FEC = fecStats
	r.report.appendErrors(fecErrs)

	tl := RebuildTimeline(recovered, cfg.FrameCount, r.expectedAudioPackets())
	r.report.Timeline = tl
	r.report.GapFrames = len(tl.GapFrames)
	r.report.RecoveredPackets = len(tl.Recovered)

	if err := r.processGroups(cp, tl); err != nil {
		return r.report, err
	}
	if err := r.finalizePlaylists(); err != nil {
		return r.report, err
	}
	return r.report, nil
}

func validateConfig(cfg Config) error {
	if cfg.FrameCount == 0 || cfg.FrameCount > ingest.MaxFrames {
		return media.NewError(media.ClassConfig, "pipeline", "frame count out of range", 0, false)
	}
	if cfg.FrameCount%uint32(cfg.FPS) != 0 {
		return media.NewError(media.ClassConfig, "pipeline", "frames must cover whole seconds", 0, false)
	}
	if cfg.SegmentMS != 1000 {
		return media.NewError(media.ClassConfig, "pipeline", "only 1000 ms segments are supported", 0, false)
	}
	if len(cfg.Targets) == 0 {
		return media.NewError(media.ClassConfig, "pipeline", "no bitrate targets", 0, false)
	}
	for _, t := range cfg.Targets {
		if t != 180 && t != 450 && t != 900 {
			return media.NewError(media.ClassConfig, "pipeline", "unsupported target: "+itoa(t), 0, false)
		}
	}
	return checkOutputDir(cfg.OutDir)
}

func (r *Runner) expectedAudioPackets() uint32 {
	seconds := cfgSeconds(r.cfg)
	return seconds * media.AudioPacketsPerSecond
}

func cfgSeconds(cfg Config) uint32 {
	return cfg.FrameCount / cfg.FPS
}

// openState creates or replays the journal and enforces checkpoint identity.
func (r *Runner) openState(cp index.Checkpoint) error {
	idxDir := filepath.Join(r.cfg.OutDir, "index")
	j, entries, err := index.OpenJournal(idxDir)
	if err != nil {
		return err
	}
	r.journal = j
	st, err := index.Load(entries)
	if err != nil {
		return err
	}
	if st.Checkpoint == nil {
		if err := j.SaveCheckpoint(cp); err != nil {
			return err
		}
		st.Checkpoint = &cp
	} else if err := st.VerifyCompatible(cp); err != nil {
		if errors.Is(err, index.ErrNoCheckpoint) {
			return media.NewError(media.ClassIndex, "pipeline", "journal without checkpoint", 0, false)
		}
		return err // input/config changed -> exit code 4
	}
	r.state = st
	r.report.ResumeSegments = len(st.Groups)
	return nil
}

func (r *Runner) readPackets() ([]*media.Packet, int, error) {
	reader, manifest, err := ingest.OpenPacketReader(r.cfg.InputPath)
	if err != nil {
		return nil, 0, err
	}
	if err := manifest.Validate(filepath.Dir(r.cfg.InputPath)); err != nil {
		reader.Close()
		return nil, 0, err
	}
	defer reader.Close()
	var out []*media.Packet
	corrupt := 0
	for {
		p, err := reader.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var me *media.Error
			if errors.As(err, &me) && me.Class == media.ClassCorrupt && p != nil {
				// Keep the corrupt record: FEC reconstructs the slot from
				// parity without trusting the damaged payload. It is marked
				// so downstream code never consumes its bytes directly.
				corrupt++
				p.Corrupt = true
				r.report.appendErrors([]*media.Error{me})
				out = append(out, p)
				continue
			}
			return nil, 0, err
		}
		out = append(out, p)
	}
	return out, corrupt, nil
}

// processGroups encodes, verifies and commits one segment group at a time.
func (r *Runner) processGroups(cp index.Checkpoint, tl *Timeline) error {
	seconds := cfgSeconds(r.cfg)
	tiers := r.tiers()
	ctrl := abr.NewController(4)
	stagingRoot := filepath.Join(r.cfg.OutDir, "staging")

	for g := uint32(0); g < seconds; g++ {
		if int(g) < r.report.ResumeSegments {
			r.report.Segments = append(r.report.Segments, r.loadCommitted(g, tiers, tl)...)
			continue
		}
		ctrl.Observe(r.bandwidthSample(g))
		decision := ctrl.Decide()
		groupDir := filepath.Join(stagingRoot, fmt.Sprintf("g%d", g))
		segs, err := r.encodeGroup(g, groupDir, tl, tiers, decision)
		if err != nil {
			return err
		}
		hashes, err := r.publishGroup(g, segs, tiers)
		if err != nil {
			return err
		}
		if err := index.EnsureGroupConsistent(hashes, len(tiers)); err != nil {
			return err
		}
		gapped := groupHasGap(tl, g)
		rec := index.GroupRecord{
			GroupIndex: int(g), SegmentHashes: hashes, Gapped: gapped,
		}
		if err := r.journal.CommitGroup(rec); err != nil {
			return err
		}
		// Keep the in-memory state in sync with the journal so playlist
		// finalization sees every committed group without replaying disk.
		r.state.Groups = append(r.state.Groups, rec)
		r.report.Segments = append(r.report.Segments, segs...)
		if r.cfg.InterruptAfter > 0 && int(g)+1 == r.cfg.InterruptAfter && !r.cfg.Resume {
			return ErrInterrupted
		}
	}
	return nil
}

func (r *Runner) tiers() []segment.Tier {
	var out []segment.Tier
	for _, kbps := range r.cfg.Targets {
		for _, t := range segment.Tiers {
			if t.Kbps == kbps {
				out = append(out, t)
			}
		}
	}
	return out
}

// bandwidthSample is the deterministic, time-varying network trace.
func (r *Runner) bandwidthSample(g uint32) int64 {
	trace := []int64{2_200_000, 700_000, 280_000, 1_500_000, 900_000, 200_000, 1_800_000, 600_000}
	base := trace[int(g)%len(trace)]
	jitter := int64(r.rng.Intn(40_000)) - 20_000
	return base + jitter
}

func groupHasGap(tl *Timeline, g uint32) bool {
	for f := g * uint32(media.VideoFPS); f < (g+1)*uint32(media.VideoFPS); f++ {
		if tl.GapFrames[f] {
			return true
		}
	}
	return false
}

func hashFile(path string) ([32]byte, error) {
	var out [32]byte
	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return out, err
	}
	copy(out[:], h.Sum(nil))
	return out, nil
}

// HashHex is a convenience for reports and tests.
func HashHex(sum [32]byte) string { return hex.EncodeToString(sum[:]) }
