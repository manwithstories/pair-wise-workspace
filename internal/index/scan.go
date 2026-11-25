// Package index scans media assets and persists digest-keyed sample indexes.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"github.com/abema/go-mp4"

	"bcast/internal/fault"
	"bcast/internal/timebase"
)

// Sample is one exact MP4 sample-table entry.
type Sample struct {
	Index      uint32            `json:"index"`
	Size       uint32            `json:"size"`
	Offset     int64             `json:"offset"`
	DTS        uint64            `json:"dts"`
	PTS        int64             `json:"pts"`
	Duration   uint32            `json:"duration"`
	Keyframe   bool              `json:"keyframe"`
	PTSSeconds timebase.Rational `json:"pts_seconds"`
	DTSSeconds timebase.Rational `json:"dts_seconds"`
}

// TrackIndex contains one track's sample, PTS/DTS and keyframe tables.
type TrackIndex struct {
	TrackID   uint32   `json:"track_id"`
	Timescale uint32   `json:"timescale"`
	Duration  uint64   `json:"duration"`
	Codec     int      `json:"codec"`
	Samples   []Sample `json:"samples"`
	Keyframes []uint32 `json:"keyframes"` // 1-based MP4 sample numbers
}

// AssetIndex is the SHA-256-keyed scan result for one MP4 or RTP sidecar.
type AssetIndex struct {
	Path       string       `json:"path"`
	SHA256     string       `json:"sha256"`
	Size       int64        `json:"size"`
	MP4        bool         `json:"mp4"`
	Tracks     []TrackIndex `json:"tracks,omitempty"`
	RTPReport  any          `json:"rtp_report,omitempty"`
	Rejected   bool         `json:"rejected,omitempty"`
	ReasonCode string       `json:"reason_code,omitempty"`
	Reason     string       `json:"reason,omitempty"`
}

// ScanOptions controls bounded validation behavior.
type ScanOptions struct {
	// ReadSampleBytes additionally hashes sample byte ranges. It is off for
	// normal indexing because SHA-256 of the whole input already binds bytes.
	ReadSampleBytes bool
}

// ScanMP4 hashes and parses an MP4 while validating sample bounds and PTS.
func ScanMP4(path, rel string, opts ScanOptions) (*AssetIndex, error) {
	sum, size, err := hashFile(path)
	if err != nil {
		return nil, fault.E(fault.AssetDigest, "index", "ScanMP4.hash", err).WithPath(rel)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fault.E(fault.Internal, "index", "ScanMP4.open", err).WithPath(rel)
	}
	defer f.Close()

	info, err := mp4.Probe(f)
	if err != nil {
		return nil, fault.E(fault.MP4Corrupt, "index", "ScanMP4.probe", err).WithPath(rel)
	}
	if len(info.Tracks) == 0 {
		return nil, fault.E(fault.MP4Corrupt, "index", "ScanMP4.tracks", errors.New("mp4 contains no tracks")).WithPath(rel)
	}
	keySets, err := extractSyncSamples(f)
	if err != nil {
		return nil, fault.E(fault.MP4Corrupt, "index", "ScanMP4.stss", err).WithPath(rel)
	}

	ai := &AssetIndex{Path: rel, SHA256: sum, Size: size, MP4: true, Tracks: make([]TrackIndex, 0, len(info.Tracks))}
	for ti, tr := range info.Tracks {
		track := TrackIndex{
			TrackID:   tr.TrackID,
			Timescale: tr.Timescale,
			Duration:  tr.Duration,
			Codec:     int(tr.Codec),
			Samples:   make([]Sample, 0, len(tr.Samples)),
		}
		var keySet map[uint32]bool
		if ti < len(keySets) {
			track.Keyframes = append(track.Keyframes, keySets[ti]...)
			keySet = make(map[uint32]bool, len(track.Keyframes))
			for _, n := range track.Keyframes {
				keySet[n] = true
			}
		} else if len(keySets) == 1 {
			track.Keyframes = append(track.Keyframes, keySets[0]...)
			keySet = make(map[uint32]bool, len(track.Keyframes))
			for _, n := range track.Keyframes {
				keySet[n] = true
			}
		}

		if err := buildSamples(&track, tr, keySet, size, rel); err != nil {
			return nil, err
		}
		if opts.ReadSampleBytes {
			if err := verifySampleBytes(f, &track, rel); err != nil {
				return nil, err
			}
		}
		ai.Tracks = append(ai.Tracks, track)
	}
	return ai, nil
}

func buildSamples(track *TrackIndex, tr *mp4.Track, keys map[uint32]bool, fileSize int64, rel string) error {
	sampleIdx := 0
	var dts uint64
	var lastPTS *int64
	for ci, chunk := range tr.Chunks {
		offset := int64(chunk.DataOffset)
		for k := uint32(0); k < chunk.SamplesPerChunk; k++ {
			if sampleIdx >= len(tr.Samples) {
				return fault.E(fault.MP4Corrupt, "index", "buildSamples", errors.New("stsc references more samples than stsz/stts")).WithPath(rel)
			}
			ms := tr.Samples[sampleIdx]
			n := uint32(sampleIdx + 1)
			end := offset + int64(ms.Size)
			if offset < 0 || end > fileSize {
				return fault.E(fault.MP4Truncated, "index", "buildSamples",
					errors.New("sample data range is outside file")).WithPath(rel).WithSegment(sampleIdx)
			}
			pts := int64(dts) + ms.CompositionTimeOffset
			if lastPTS != nil && pts < *lastPTS {
				return fault.E(fault.PTSRegression, "index", "buildSamples",
					errors.New("presentation timestamp moved backwards")).WithPath(rel).WithSegment(sampleIdx)
			}
			lastPTS = &pts
			s := Sample{
				Index: n, Size: ms.Size, Offset: offset, DTS: dts, PTS: pts,
				Duration: ms.TimeDelta, Keyframe: keys == nil || keys[n],
				PTSSeconds: timebase.SampleTime(uint64(max64(pts, 0)), track.Timescale),
				DTSSeconds: timebase.SampleTime(dts, track.Timescale),
			}
			track.Samples = append(track.Samples, s)
			dts += uint64(ms.TimeDelta)
			offset = end
			sampleIdx++
		}
		_ = ci
	}
	if sampleIdx != len(tr.Samples) {
		return fault.E(fault.MP4Corrupt, "index", "buildSamples", errors.New("not all sample sizes have chunk locations")).WithPath(rel)
	}
	return nil
}

func verifySampleBytes(f *os.File, track *TrackIndex, rel string) error {
	for _, s := range track.Samples {
		r := io.NewSectionReader(f, s.Offset, int64(s.Size))
		if _, err := io.Copy(io.Discard, r); err != nil {
			return fault.E(fault.MP4Truncated, "index", "verifySampleBytes", err).WithPath(rel).WithSegment(int(s.Index))
		}
	}
	return nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func extractSyncSamples(f io.ReadSeeker) ([][]uint32, error) {
	boxes, err := mp4.ExtractBoxesWithPayload(f, nil, []mp4.BoxPath{{
		mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStss(),
	}})
	if err != nil {
		return nil, err
	}
	out := make([][]uint32, 0, len(boxes))
	for _, bp := range boxes {
		stss, ok := bp.Payload.(*mp4.Stss)
		if !ok {
			continue
		}
		out = append(out, append([]uint32(nil), stss.SampleNumber...))
	}
	return out, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
