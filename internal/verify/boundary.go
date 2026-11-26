// Package verify performs independent index and timeline boundary checks.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"

	clockpkg "mediad/internal/clock"
	"mediad/internal/index"
	ladderpkg "mediad/internal/ladder"
	"mediad/internal/media"
	"mediad/internal/mp4"
)

// TrackReport is the per-track cross-check outcome.
type TrackReport struct {
	TrackID         uint32 `json:"track_id"`
	Kind            string `json:"kind"`
	Samples         int    `json:"samples"`
	SyncSamples     int    `json:"sync_samples"`
	IndexPoints     int    `json:"index_points"`
	VerifiedPoints  int    `json:"verified_points"`
	TimelineOrdered bool   `json:"timeline_ordered"`
	OffsetsInBounds bool   `json:"offsets_in_bounds"`
	FirstOffset     int64  `json:"first_offset"`
	LastOffsetEnd   int64  `json:"last_offset_end"`
	FrameDurationNS int64  `json:"frame_duration_ns"`
}

// Report is the independent verification summary.
type Report struct {
	Input            string         `json:"input"`
	FileSize         int64          `json:"file_size"`
	OutputSHA256     string         `json:"output_sha256"`
	Format           string         `json:"format"`
	BoxCount         int            `json:"box_count"`
	Tracks           []TrackReport  `json:"tracks"`
	IndexComplete    bool           `json:"index_complete"`
	IndexSeekMiss    int            `json:"index_seek_miss"`
	BoundaryMismatch int            `json:"boundary_mismatch"`
	SyncMaxMS        float64        `json:"sync_max_ms"`
	LadderMismatch   int            `json:"ladder_mismatch"`
	OK               bool           `json:"ok"`
	Faults           []*media.Fault `json:"faults,omitempty"`
}

// VerifyFile parses path independently, rebuilds its index, and cross-checks
// timelines, offsets, sync points, and ladder boundaries.
func VerifyFile(path string) (*Report, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, media.NewFault(media.CodeInvalidInput, "stat input: %v", err).Fatal()
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, media.NewFault(media.CodeInvalidInput, "open input: %v", err).Fatal()
	}
	defer f.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return nil, media.NewFault(media.CodeInvalidInput, "hash input: %v", err).Fatal()
	}
	sha := hex.EncodeToString(hash.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	p, boxes, parseFaults, err := mp4.ReadProject(f, st.Size(), false)
	if err != nil {
		return &Report{Input: path, FileSize: st.Size(), OutputSHA256: sha, OK: false, Faults: []*media.Fault{media.AsFaultOrInternal(err)}}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	rep := &Report{
		Input: path, FileSize: st.Size(), OutputSHA256: sha, Format: p.SourceFormat,
		BoxCount: len(boxes), Faults: parseFaults,
	}
	for _, b := range boxes {
		rep.BoxCount += len(b.Children)
	}

	idx, idxFaults := index.Rebuild(p)
	rep.Faults = append(rep.Faults, idxFaults...)
	misses, verifyFaults := idx.Verify(f, st.Size())
	rep.Faults = append(rep.Faults, verifyFaults...)
	rep.IndexSeekMiss = misses
	rep.IndexComplete = idx.Complete

	fanout, clockFaults := clockpkg.New(p, clockpkg.DefaultTimescale)
	rep.Faults = append(rep.Faults, clockFaults...)
	var videoID, audioID uint32
	var videoTrack *media.Track
	for _, t := range p.Tracks {
		tr := TrackReport{TrackID: t.ID, Kind: string(t.Kind), Samples: len(t.Samples), TimelineOrdered: true, OffsetsInBounds: true}
		tr.SyncSamples = len(t.SyncSampleNumbers())
		if ti := idx.Tracks[t.ID]; ti != nil {
			tr.IndexPoints = len(ti.Points)
			for _, pt := range ti.Points {
				if pt.Verified {
					tr.VerifiedPoints++
				}
			}
		}
		if len(t.Samples) > 0 {
			tr.FirstOffset = t.Samples[0].Offset
			last := t.Samples[len(t.Samples)-1]
			tr.LastOffsetEnd = last.Offset + int64(last.Size)
		}
		for i, s := range t.Samples {
			if i > 0 && (s.DTS < t.Samples[i-1].DTS || s.PTS < t.Samples[i-1].PTS) {
				tr.TimelineOrdered = false
				rep.BoundaryMismatch++
			}
			end, ok := media.ByteRange{Offset: s.Offset, Length: int64(s.Size)}.End()
			if !ok || s.Offset < 0 || end > st.Size() {
				tr.OffsetsInBounds = false
				rep.BoundaryMismatch++
			}
		}
		if ns, err := t.FrameDurationNS(); err == nil {
			tr.FrameDurationNS = ns
		}
		if !tr.TimelineOrdered || !tr.OffsetsInBounds || tr.VerifiedPoints != tr.SyncSamples {
			rep.BoundaryMismatch++
		}
		rep.Tracks = append(rep.Tracks, tr)
		switch t.Kind {
		case media.KindVideo:
			if videoTrack == nil {
				videoTrack, videoID = t, t.ID
			}
		case media.KindAudio:
			if audioID == 0 {
				audioID = t.ID
			}
		}
	}

	if videoID != 0 && audioID != 0 {
		gapNS := fanout.BoundaryGapNS(videoID, audioID)
		rep.SyncMaxMS = float64(gapNS) / 1e6
		// A/V nearest-boundary gap must stay within one 30 fps frame plus 1ms.
		if gapNS > 34_333_333 {
			rep.BoundaryMismatch++
			rep.Faults = append(rep.Faults, media.NewFault(media.CodeBoundaryMismatch, "av boundary gap %.3fms exceeds one frame", rep.SyncMaxMS).Fatal())
		}
	}

	if videoTrack != nil {
		ladder, ladderFaults := ladderpkg.Build(videoTrack, ladderpkg.DefaultLevels())
		rep.Faults = append(rep.Faults, ladderFaults...)
		if ladder != nil {
			rep.LadderMismatch = ladder.MismatchCount()
			rep.BoundaryMismatch += rep.LadderMismatch
			if len(ladder.Renditions) >= 2 && len(ladder.Renditions[0].Boundaries) > 0 {
				n := ladder.Renditions[0].Boundaries[0].SampleNumber
				if _, fault := ladder.CheckSwitch(0, 1, n); fault != nil {
					rep.BoundaryMismatch++
					rep.Faults = append(rep.Faults, fault)
				}
			}
		}
	}

	rep.OK = rep.IndexComplete && rep.IndexSeekMiss == 0 && rep.BoundaryMismatch == 0 && !hasFatalFault(rep.Faults)
	return rep, nil
}

func hasFatalFault(fs []*media.Fault) bool {
	for _, f := range fs {
		if f != nil && !f.Recoverable {
			return true
		}
	}
	return false
}
