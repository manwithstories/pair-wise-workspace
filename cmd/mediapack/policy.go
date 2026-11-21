package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediapack/internal/audio"
	"mediapack/internal/color"
)

func hashArtifactsMatch(a, b hashArtifact) bool {
	if len(a.Segments) != len(b.Segments) {
		return false
	}
	for i := range a.Segments {
		if a.Segments[i].Ordinal != b.Segments[i].Ordinal || a.Segments[i].SHA256 != b.Segments[i].SHA256 {
			return false
		}
	}
	return true
}

func (rs *runState) configHash() string {
	payload := fmt.Sprintf("seed=%d;rates=%v;ticks=%d;frames=%d;inject=%s;audio=%s;color=%s",
		rs.opts.Seed, rs.opts.Rates, rs.opts.SegmentTick, rs.opts.FrameCount,
		rs.opts.Inject, rs.opts.Policies.Audio, rs.opts.Policies.Color)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// fixtureInputs returns one stable hash per representation.
func (rs *runState) fixtureInputs() []string {
	var out []string
	for _, r := range rs.bundle.Rates {
		out = append(out, r.FileHash)
	}
	return out
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func jsonBytes(v any) []byte {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// audioConfig and colorConfig parse the CLI policy strings into stage config.
func audioConfig(s string) audio.Config {
	switch {
	case s == "" || s == "block":
		return audio.Config{Policy: audio.PolicyBlock}
	case len(s) > 7 && s[:7] == "prefer:":
		return audio.Config{Policy: audio.PolicyPrefer, PreferSource: s[7:]}
	case len(s) > 8 && s[:8] == "downmix:":
		return audio.Config{Policy: audio.PolicyMapDown, Target: audio.ParseLayout(s[8:])}
	}
	return audio.Config{Policy: audio.PolicyBlock}
}

func colorConfig(s string) color.Config {
	switch {
	case s == "" || s == "block":
		return color.Config{Policy: color.PolicyBlock}
	case len(s) > 7 && s[:7] == "prefer:":
		return color.Config{Policy: color.PolicyPrefer, PreferSource: s[7:]}
	}
	return color.Config{Policy: color.PolicyBlock}
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
