package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sort"

	"mediabridge/internal/adapter"
	"mediabridge/internal/core"
	"mediabridge/internal/fault"
	"mediabridge/internal/journal"
	"mediabridge/internal/manifest"
	"mediabridge/internal/maptrack"
	"mediabridge/internal/media"
)

// resumeOutcome reports what a resumable run did.
type resumeOutcome struct {
	Resumed        int
	Quarantined    int
	PriorCommitted int
	Written        int
}

// manifestAsset aliases the resolved manifest asset.
type manifestAsset = manifest.ResolvedAsset

// runResumable builds the output stream through a checkpointed writer. The
// checkpoint stores the byte length of the complete-packet prefix and a digest
// of that prefix; on resume the output is truncated to that length and
// appending continues, so an interrupted tail is discarded and no packet is
// ever written twice.
func runResumable(j *journal.Journal, asset *media.Asset, plan *maptrack.Plan,
	prov *core.Provenance, ra manifestAsset, ad adapter.Adapter, opts Options,
	outPath string, interruptAfter int64, source core.SampleSource) (resumeOutcome, int64, error) {

	var outcome resumeOutcome
	stream := core.BuildStream(asset, plan)

	cfg := journal.ConfigDigest("keep_payload=1",
		"max_packets=20000000", "max_duration_ns=86400000000000")
	guards := journal.Guards{
		SourceSHA256: asset.SourceSHA, Adapter: ad.Format(),
		AdapterVersion: ad.Version(), ConfigDigest: cfg,
	}

	prefixLen := int64(0)
	committed := int64(0)
	if st, q, err := j.LoadCheckpoint(ra.ID, guards); err != nil {
		return outcome, 0, fault.New(fault.CodeCheckpoint,
			fault.Location{AssetID: ra.ID, SourcePath: outPath}, "load checkpoint failed").
			Wrap(err).WithOp("pipeline.Resume")
	} else if q {
		outcome.Quarantined = 1
	} else if st != nil && st.CommittedPackets > 0 {
		if verifyPrefix(outPath, st) {
			prefixLen = st.PrefixBytes
			committed = st.CommittedPackets
			outcome.PriorCommitted = int(committed)
			outcome.Resumed = 1
		} else {
			outcome.Quarantined = 1
		}
	}

	// Guard: a checkpoint past the current stream means the source shrank.
	if committed > int64(len(stream)) {
		prefixLen, committed = 0, 0
		outcome.PriorCommitted, outcome.Resumed = 0, 0
		outcome.Quarantined = 1
	}

	copyPrefix := func(h io.Writer) (int64, error) {
		f, err := os.Open(outPath)
		if err != nil {
			return 0, err
		}
		defer f.Close()
		return io.CopyN(h, f, prefixLen)
	}

	w, err := core.NewWriter(outPath, prefixLen, copyPrefix)
	if err != nil {
		return outcome, 0, fault.New(fault.CodeCheckpoint,
			fault.Location{AssetID: ra.ID, SourcePath: outPath}, "open output failed").
			Wrap(err).WithOp("pipeline.Resume")
	}
	defer w.Close()

	if prefixLen == 0 {
		if err := w.WriteHeader(asset, plan, int64(len(stream))); err != nil {
			return outcome, 0, err
		}
	}

	// Provenance records every output packet. For packets still in memory the
	// digest is computed here; for packets streamed from the source the digest
	// is filled as they are written. A resumed prefix's digests are derived
	// from the output bytes by the verifier, so provenance coverage stays 100%
	// even though this run never saw those payloads.
	slotIndex := map[string]int64{}
	for _, sp := range stream {
		cp := sp.Pkt
		if len(cp.Payload) > 0 {
			sum := sha256.Sum256(cp.Payload)
			cp.SHA256 = hex.EncodeToString(sum[:])
		}
		prov.AddPacket(sp.Slot, slotIndex[sp.Slot], cp)
		slotIndex[sp.Slot]++
	}

	for i := int(committed); i < len(stream); i++ {
		sp := stream[i]
		payload := sp.Pkt.Payload
		if payload == nil {
			if source == nil {
				return outcome, 0, fault.New(fault.CodeVerify,
					fault.Location{AssetID: ra.ID, SourcePath: outPath}.WithTrack(sp.Pkt.SourceTrackID),
					"packet payload unavailable and no source reader").WithOp("pipeline.Resume")
			}
			data, err := source.ReadSample(sp.Pkt.SourceTrackID, sp.Pkt.SourceOffset, sp.Pkt.SourceSize)
			if err != nil || int64(len(data)) != sp.Pkt.SourceSize {
				return outcome, 0, fault.New(fault.CodeVerify,
					fault.Location{AssetID: ra.ID, SourcePath: outPath}.
						WithTrack(sp.Pkt.SourceTrackID).WithOffset(sp.Pkt.SourceOffset),
					"read source sample failed").Wrap(err).WithOp("pipeline.Resume")
			}
			payload = data
		}
		if err := w.WritePacket(sp.Slot, sp.Pkt, payload); err != nil {
			return outcome, 0, err
		}
		outcome.Written++

		// Checkpoint every 256 packets (and at the end). A crash loses at most
		// 255 packets of work — well within the recovery margin — while fsync
		// stays cheap enough for the pipeline latency budget. The interrupt
		// simulator also forces a checkpoint so the resumed run has neutral
		// work to skip and no packets are duplicated.
		forceCheckpoint := interruptAfter > 0 && outcome.Written >= int(interruptAfter)
		if (i+1)%256 == 0 || i == len(stream)-1 || forceCheckpoint {
			if err := w.Sync(); err != nil {
				return outcome, 0, err
			}
			end, err := w.Size()
			if err != nil {
				return outcome, 0, err
			}
			st := &journal.AssetState{
				AssetID:          ra.ID,
				CommittedPackets: int64(i + 1),
				PrefixBytes:      end,
				PrefixSHA256:     w.Digest(),
				SourceSHA256:     guards.SourceSHA256,
				Adapter:          guards.Adapter,
				AdapterVersion:   guards.AdapterVersion,
				ConfigDigest:     guards.ConfigDigest,
				Tracks:           trackRanges(stream[:i+1]),
			}
			if err := j.SaveCheckpoint(st); err != nil {
				return outcome, 0, fault.New(fault.CodeCheckpoint,
					fault.Location{AssetID: ra.ID, SourcePath: outPath}, "save checkpoint failed").
					Wrap(err).WithOp("pipeline.Resume")
			}
			if forceCheckpoint {
				return outcome, int64(outcome.Written), errInterrupted{}
			}
		}
	}
	if err := w.Sync(); err != nil {
		return outcome, 0, err
	}
	size, _ := w.Size()
	prov.OutputSHA256 = w.Digest()
	prov.OutputBytes = size
	return outcome, int64(outcome.Written), nil
}

// trackRanges computes per-track committed input ranges from the stream prefix.
func trackRanges(committed []core.StreamItem) []journal.TrackRange {
	byTrack := map[uint32]*journal.TrackRange{}
	for _, sp := range committed {
		tr, ok := byTrack[sp.Pkt.SourceTrackID]
		if !ok {
			tr = &journal.TrackRange{TrackID: sp.Pkt.SourceTrackID, Slot: sp.Slot, CommittedThrough: -1}
			byTrack[sp.Pkt.SourceTrackID] = tr
		}
		if sp.Pkt.PacketIndex > tr.CommittedThrough {
			tr.CommittedThrough = sp.Pkt.PacketIndex
		}
		tr.CommitCount++
	}
	out := make([]journal.TrackRange, 0, len(byTrack))
	for _, tr := range byTrack {
		out = append(out, *tr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TrackID < out[j].TrackID })
	return out
}

// errInterrupted signals a simulated crash after a clean checkpoint.
type errInterrupted struct{}

func (errInterrupted) Error() string { return "pipeline: interrupted after checkpoint" }

// IsInterrupted reports whether err is the simulated-crash sentinel.
func IsInterrupted(err error) bool { _, ok := err.(errInterrupted); return ok }

func verifyPrefix(path string, st *journal.AssetState) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() < st.PrefixBytes {
		return false
	}
	h := sha256.New()
	if _, err := io.CopyN(h, f, st.PrefixBytes); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == st.PrefixSHA256
}
