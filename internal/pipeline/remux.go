package pipeline

import (
	"fmt"
	"os"
	"path/filepath"

	"mediaqa/internal/manifest"
	"mediaqa/internal/media"
	mp4scan "mediaqa/internal/mp4"
	"mediaqa/internal/report"
)

// remux writes a new MP4 containing only the valid repaired samples.
// Payload offsets and lengths are verified against the source before
// copying; the output is written atomically and re-scanned so the index
// references the new file's real offsets.
func (tr *TrackResult) remux(cfg Config, m manifest.Track, srcPath string, repaired []mp4scan.Sample, ft *mp4scan.Track, el *media.List) error {
	if len(repaired) == 0 {
		return nil // nothing recoverable; no output produced
	}
	src, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("read source: %w", err)
	}
	out := mp4scan.OutTrack{
		ID:        ft.ID,
		Kind:      ft.Kind,
		Timescale: uint32(ft.Timescale),
	}
	for i, s := range repaired {
		end := s.Offset + uint64(s.Size)
		if end > uint64(len(src)) || end < s.Offset {
			return fmt.Errorf("sample %d offset %d size %d outside source (%d bytes)",
				s.Index, s.Offset, s.Size, len(src))
		}
		dur := s.Duration
		if i+1 < len(repaired) {
			if d := repaired[i+1].DTS - s.DTS; d > 0 {
				dur = d
			}
		}
		ct := s.PTS - s.DTS
		out.Samples = append(out.Samples, mp4scan.OutSample{
			Duration: uint32(dur),
			Key:      s.Key,
			CTOffset: int32(ct),
			Payload:  src[s.Offset:end],
		})
	}
	data, err := mp4scan.MarshalMP4([]mp4scan.OutTrack{out})
	if err != nil {
		return err
	}
	dst := filepath.Join(cfg.OutDir, m.ID+".mp4")
	if err := report.AtomicWrite(dst, data); err != nil {
		return err
	}
	tr.RemuxPath = dst
	// Re-scan our own output to obtain authoritative offsets.
	rescan, err := mp4scan.Scan(dst, el)
	if err != nil {
		return fmt.Errorf("rescan remux: %w", err)
	}
	for i := range rescan.Tracks {
		if rescan.Tracks[i].Kind == ft.Kind {
			tr.remuxed = rescan.Tracks[i].Samples
			break
		}
	}
	if len(tr.remuxed) != len(repaired) {
		return fmt.Errorf("remux rescan yielded %d samples, want %d", len(tr.remuxed), len(repaired))
	}
	return nil
}
