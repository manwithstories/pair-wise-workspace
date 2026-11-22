package scan

import (
	"fmt"

	"mediaslice/media"
)

// FingerprintMismatch explains why a cached index was rejected.
type FingerprintMismatch struct {
	Field    string `json:"field"`
	Expected string `json:"expected"`
	Got      string `json:"got"`
}

func (m *FingerprintMismatch) Error() string {
	return fmt.Sprintf("index fingerprint mismatch on %s: got %s want %s",
		m.Field, m.Got, m.Expected)
}

// VerifyIndex validates a cached index against the current source file. It
// recomputes the input SHA-256, checks the parser version and re-derives
// every per-track summary digest. The first mismatch is returned.
func VerifyIndex(idx *FileIndex, sourcePath string) *FingerprintMismatch {
	sum, size, err := HashFile(sourcePath)
	if err != nil {
		return &FingerprintMismatch{Field: "input_sha256", Expected: "<unreadable>", Got: err.Error()}
	}
	if idx.ParserVersion != ParserVersion {
		return &FingerprintMismatch{
			Field: "parser_version", Expected: ParserVersion, Got: idx.ParserVersion,
		}
	}
	if idx.InputSHA256 != sum {
		return &FingerprintMismatch{Field: "input_sha256", Expected: sum, Got: idx.InputSHA256}
	}
	if idx.FileSize != size {
		return &FingerprintMismatch{Field: "file_size",
			Expected: fmt.Sprintf("%d", size), Got: fmt.Sprintf("%d", idx.FileSize)}
	}
	for i := range idx.Tracks {
		got := summarize(&idx.Tracks[i])
		want := idx.Summaries[i]
		if got.SampleDigest != want.SampleDigest {
			return &FingerprintMismatch{
				Field:    fmt.Sprintf("track[%d].digest", i),
				Expected: want.SampleDigest,
				Got:      got.SampleDigest,
			}
		}
		if got.SampleCount != want.SampleCount {
			return &FingerprintMismatch{
				Field:    fmt.Sprintf("track[%d].sample_count", i),
				Expected: fmt.Sprintf("%d", want.SampleCount),
				Got:      fmt.Sprintf("%d", got.SampleCount),
			}
		}
	}
	return nil
}

// ScanOrLoad returns a verified cached index when present, otherwise scans
// the source, persists the new index and returns it.
func ScanOrLoad(workDir, mp4Path, rtpPath string, cfg Config) (*FileIndex, bool, error) {
	sum, _, err := HashFile(mp4Path)
	if err != nil {
		return nil, false, err
	}
	indexPath, err := SafeJoin(workDir, "index/"+IndexFileName(sum))
	if err != nil {
		return nil, false, err
	}
	if cached, err := ReadIndex(indexPath); err == nil {
		if VerifyIndex(cached, mp4Path) == nil {
			return cached, true, nil
		}
		// Stale index: fall through and rebuild.
	}
	idx, err := ScanFile(mp4Path, rtpPath, cfg)
	if err != nil {
		return nil, false, err
	}
	if _, err := WriteIndex(workDir, idx); err != nil {
		return nil, false, err
	}
	return idx, false, nil
}

// FirstFatal returns the first non-recoverable media error, if any.
func (idx *FileIndex) FirstFatal() *media.MediaError {
	if len(idx.Errors) == 0 {
		return nil
	}
	return idx.Errors[0]
}
