package demo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"mediacore/internal/fixture"
)

// InputDigest hashes every input file (manifests, segments, RTP
// sample) into one deterministic digest.
func InputDigest(dir string, fix *fixture.Result) (string, error) {
	files := append(append([]string{}, fix.Manifests...), fix.SegmentFiles...)
	files = append(files, "rtp.bin")
	sort.Strings(files)
	h := sha256.New()
	for _, name := range files {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s:", name)
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			return "", err
		}
		f.Close()
		fmt.Fprintln(h)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ParamsDigest hashes the digest-relevant parameters.
func ParamsDigest(p Params) string {
	data, _ := json.Marshal(struct {
		Seed     int64   `json:"seed"`
		CutStart float64 `json:"cut_start"`
		CutEnd   float64 `json:"cut_end"`
		Version  string  `json:"version"`
	}{p.Seed, p.CutStart, p.CutEnd, Version})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
