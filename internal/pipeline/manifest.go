package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/example/mediad/internal/audio"
	"github.com/example/mediad/internal/ingest"
	"github.com/example/mediad/internal/verify"
)

// quarantineDir holds inputs rejected during parsing or mapping.
const quarantineDir = "quarantine"

// quarantineIfFails parses path; on failure it moves the file into the
// quarantine area and returns a rejected manifest record.
func quarantineIfFails(path, reason string) (verify.SegmentRecord, bool) {
	f, err := os.Open(path)
	if err != nil {
		return verify.SegmentRecord{}, false
	}
	if _, err := ingest.ParseMP4(f, path); err == nil {
		f.Close()
		return verify.SegmentRecord{}, false
	}
	f.Close()

	work := filepath.Dir(filepath.Dir(path))
	dst := filepath.Join(work, quarantineDir, filepath.Base(path))
	_ = os.MkdirAll(filepath.Dir(dst), 0o755)
	if err := os.Rename(path, dst); err == nil {
		path = dst
	}
	return verify.SegmentRecord{
		ID: "rejected-" + reason, Path: path, Rejected: true,
		RejectReason: reason,
	}, true
}

// quarantineBadLayout parses a fixture whose audio track disagrees with
// its channel labels and rejects it through the audio validator.
func quarantineBadLayout(path string) (verify.SegmentRecord, bool) {
	f, err := os.Open(path)
	if err != nil {
		return verify.SegmentRecord{}, false
	}
	sum, err := ingest.ParseMP4(f, path)
	f.Close()
	if err != nil || sum == nil {
		return verify.SegmentRecord{}, false
	}
	for _, tr := range sum.Tracks {
		if tr.Kind != "audio" {
			continue
		}
		if err := audio.ValidateLayout(tr.Audio); err != nil {
			work := filepath.Dir(filepath.Dir(path))
			dst := filepath.Join(work, quarantineDir, filepath.Base(path))
			_ = os.MkdirAll(filepath.Dir(dst), 0o755)
			if mvErr := os.Rename(path, dst); mvErr == nil {
				path = dst
			}
			return verify.SegmentRecord{
				ID: "rejected-bad-audio-layout", Path: path,
				Rejected: true, RejectReason: "abnormal channel layout",
			}, true
		}
	}
	return verify.SegmentRecord{}, false
}

// ManifestFile is the on-disk manifest envelope.
type ManifestFile struct {
	Segments []verify.SegmentRecord `json:"segments"`
	Rejected []verify.SegmentRecord `json:"rejected"`
}

func writeManifest(work string, res *Result) error {
	doc := ManifestFile{
		Segments: res.Manifest.Segments,
		Rejected: res.Rejected,
	}
	out := filepath.Join(work, "artifacts", "manifest.json")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(out, data, 0o644)
}

// resetManagedDirs removes every pipeline-owned directory so a rerun is
// deterministic; generated inputs are recreated afterwards.
func resetManagedDirs(work string) error {
	for _, d := range []string{"input", "tmp", "segments", "txn",
		"artifacts", quarantineDir} {
		if err := os.RemoveAll(filepath.Join(work, d)); err != nil {
			return err
		}
	}
	return os.MkdirAll(filepath.Join(work, "artifacts"), 0o755)
}
