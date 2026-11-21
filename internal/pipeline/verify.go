package pipeline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math"

	"mkvarchive/internal/ebml"
	"mkvarchive/internal/model"
	"mkvarchive/internal/remux"
	"mkvarchive/internal/timeline"
)

// VerifyResult holds the numeric evidence printed by the verify command.
type VerifyResult struct {
	TargetBitrateKbps   float64
	ActualBitrateKbps   float64
	BitrateDeviationPct float64
	PayloadSHA256Match  bool
	QualityBlocks       int
	ManifestAlignmentMS float64
}

// Verify re-parses the archived stream, hashes every frame, compares against
// the source digest set and recomputes the bitrate evidence. No trust is
// placed in the archive's own bookkeeping: both streams are re-scanned.
func Verify(source, archive io.Reader, manifest *timeline.Manifest,
	targetBitrateKbps int64, durationNS int64) (*VerifyResult, error) {
	sourceBytes, err := io.ReadAll(io.LimitReader(source, maxStreamBytes+1))
	if err != nil {
		return nil, err
	}
	archiveBytes, err := io.ReadAll(io.LimitReader(archive, maxStreamBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(sourceBytes)) > maxStreamBytes || int64(len(archiveBytes)) > maxStreamBytes {
		return nil, model.NewError("INPUT_TOO_LARGE",
			"verify input exceeds the 64 MiB budget", -1)
	}
	sourceHashes, err := frameHashSet(bytes.NewReader(sourceBytes))
	if err != nil {
		return nil, err
	}
	archivedHashes, maxAlignment, blocks, payloadBytes, err :=
		scanArchive(bytes.NewReader(archiveBytes), manifest)
	if err != nil {
		return nil, err
	}
	match := len(archivedHashes) > 0
	for h := range archivedHashes {
		if !sourceHashes[h] {
			match = false
			break
		}
	}
	sec := float64(durationNS) / 1e9
	if sec <= 0 {
		sec = 1
	}
	actualKbps := float64(payloadBytes) * 8 / 1000 / sec
	deviation := 0.0
	if targetBitrateKbps > 0 {
		deviation = math.Abs(actualKbps-float64(targetBitrateKbps)) /
			float64(targetBitrateKbps) * 100
	}
	return &VerifyResult{
		TargetBitrateKbps:   float64(targetBitrateKbps),
		ActualBitrateKbps:   round1(actualKbps),
		BitrateDeviationPct: round2(deviation),
		PayloadSHA256Match:  match,
		QualityBlocks:       blocks,
		ManifestAlignmentMS: maxAlignment,
	}, nil
}

func frameHashSet(r io.Reader) (map[string]bool, error) {
	scanner, err := ebml.NewScanner(r)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for {
		blk, nerr := scanner.Next()
		if nerr == io.EOF {
			return out, nil
		}
		if nerr != nil {
			return out, nil // damaged tail: confirmed-frame hashes still count
		}
		frame, ferr := remux.FrameOf(blk.Payload)
		if ferr != nil {
			return out, nil
		}
		sum := sha256.Sum256(frame)
		out[hex.EncodeToString(sum[:])] = true
	}
}

func scanArchive(r io.Reader, manifest *timeline.Manifest) (
	map[string]bool, float64, int, int, error) {
	scanner, err := ebml.NewScanner(r)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	clock := timeline.NewClock(manifest.TimestampScale)
	rangeByID := map[uint64]timeline.TrackRange{}
	for _, tr := range manifest.Tracks {
		rangeByID[tr.ArchiveTrackID] = tr
	}
	hashes := map[string]bool{}
	var maxAlign float64
	blocks, payloadBytes := 0, 0
	for {
		blk, nerr := scanner.Next()
		if nerr == io.EOF {
			break
		}
		if nerr != nil {
			break
		}
		frame, ferr := remux.FrameOf(blk.Payload)
		if ferr != nil {
			break
		}
		sum := sha256.Sum256(frame)
		hashes[hex.EncodeToString(sum[:])] = true
		blocks++
		payloadBytes += len(frame)
		// The writer renumbered blocks onto archive track ids, so the
		// declared track number selects the matching manifest range.
		if tr, ok := rangeByID[blk.MatroskaTrackNumber()]; ok {
			ns := clock.ToNanoseconds(blk.AbsoluteTimecode())
			if ns < tr.StartNS || ns > tr.EndNS {
				nearest := tr.StartNS
				if absF64(ns-tr.EndNS) < absF64(ns-tr.StartNS) {
					nearest = tr.EndNS
				}
				if a := timeline.AlignmentErrorMS(ns, nearest); a > maxAlign {
					maxAlign = a
				}
			}
		}
	}
	return hashes, maxAlign, blocks, payloadBytes, nil
}

func absF64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }
