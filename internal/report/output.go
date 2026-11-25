package report

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"avcheck/internal/media"
)

// Encode renders the manifest as canonical, indented JSON without the hash
// field so the hash can be computed over the exact bytes that precede it.
func (m *Manifest) Encode() ([]byte, error) {
	m.SHA256 = ""
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	return body, nil
}

// Digest returns the lowercase hex SHA-256 of the encoded manifest without
// its hash field. The caller stores the digest inside the committed file.
func (m *Manifest) Digest() (string, error) {
	body, err := m.Encode()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// WriteAtomic writes manifest.json plus a copy of its digest into outDir. The
// temporary file lives in the destination directory so rename is atomic; a
// partial write never becomes visible as manifest.json.
func WriteAtomic(outDir string, m *Manifest) (string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	digest, err := m.Digest()
	if err != nil {
		return "", err
	}
	m.SHA256 = digest
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	body = append(body, '\n')
	tmp, err := os.CreateTemp(outDir, ".manifest-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	cleanup := func() { os.Remove(tmpName) }
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		cleanup()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", err
	}
	final := filepath.Join(outDir, "manifest.json")
	if err := os.Rename(tmpName, final); err != nil {
		cleanup()
		return "", err
	}
	return final, nil
}

// Diagnostics renders a human-readable fault report for a corrupt input. It
// is written instead of a manifest so no "final" output exists for a file
// that could not be fully processed.
func Diagnostics(faults *media.Faults) string {
	var b strings.Builder
	b.WriteString("avcheck diagnostics\n")
	b.WriteString("status=not_committed reason=input_corrupt\n")
	if faults == nil || faults.Empty() {
		b.WriteString("fault=<none>\n")
		return b.String()
	}
	for _, f := range faults.List {
		fmt.Fprintf(&b, "fault=%s level=%s track=%d offset=%d detail=%q\n",
			f.Code, f.Level, f.Track, f.Offset, f.Detail)
	}
	return b.String()
}

// SummaryLine is the one-line terminal summary shared by demo and check.
func (m *Manifest) SummaryLine() string {
	return fmt.Sprintf(
		"target_bps=%d actual_bps=%d delta_pct=%.2f recovered_packets=%d "+
			"quality.video_gap_pct=%.2f quality.audio_gap_ms=%.2f "+
			"av_skew_p95_ms=%.2f manifest_delta_ms=%.2f peak_buffer_bytes=%d sha256=%s",
		m.TargetBps, m.ActualBps, m.DeltaPct, m.RecoveredPackets,
		m.Quality.VideoGapPct, m.Quality.AudioGapMs,
		m.AVSkewP95Ms, m.ManifestDeltaMs, m.PeakBufferBytes, m.SHA256)
}
