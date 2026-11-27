package stream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"aircheck/internal/clock"
	"aircheck/internal/fault"
)

// measureSegments lists tmp segments in order and reads each segment's
// video-stream duration onto the 90 kHz base.
func measureSegments(dir string) ([]segState, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".ts") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var out []segState
	for _, n := range names {
		dur, err := segmentDuration(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		if err := clock.CheckDuration(dur, n); err != nil {
			return nil, err
		}
		out = append(out, segState{URI: n, DurationTicks: dur})
	}
	return out, nil
}

// segmentDuration probes the video stream duration of one segment.
func segmentDuration(path string) (int64, error) {
	out, err := execOutput("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=duration", "-of", "csv=p=0", path)
	if err != nil {
		return 0, err
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
	if err != nil {
		return 0, fmt.Errorf("bad segment duration %q: %w", out, err)
	}
	return int64(f*float64(clock.Timescale) + 0.5), nil
}

// commitSegments moves verified segments into the final directory.
// With resume, a final segment whose SHA-256 matches the fresh tmp
// segment (and the recorded state) is kept in place; anything else is
// atomically replaced. State is persisted after each commit.
func commitSegments(finalDir, tmpDir string, segs []segState, key string, resume bool) error {
	if err := os.MkdirAll(finalDir, 0o755); err != nil {
		return err
	}
	statePath := filepath.Join(finalDir, ".state.json")
	var prev runState
	if resume {
		if raw, err := os.ReadFile(statePath); err == nil {
			if jsonErr := json.Unmarshal(raw, &prev); jsonErr != nil || prev.Key != key {
				prev = runState{} // stale or corrupt state: rebuild all
			}
		}
	}
	prevHash := map[string]string{}
	for _, s := range prev.Segments {
		prevHash[s.URI] = s.SHA256
	}
	st := runState{Key: key}
	for i := range segs {
		s := &segs[i]
		tmpPath := filepath.Join(tmpDir, s.URI)
		h, err := hashFile(tmpPath)
		if err != nil {
			return err
		}
		s.SHA256 = h
		finalPath := filepath.Join(finalDir, s.URI)
		reuse := false
		if resume && prevHash[s.URI] == h {
			if fh, err := hashFile(finalPath); err == nil && fh == h {
				reuse = true
			}
		}
		if !reuse {
			if err := os.Rename(tmpPath, finalPath); err != nil {
				return err
			}
		}
		st.Segments = append(st.Segments, *s)
		if err := saveState(statePath, &st); err != nil {
			return err
		}
	}
	return nil
}

func saveState(path string, st *runState) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fault.New(fault.CodeSegmentCorrupt, filepath.Base(path), -1, true,
				"segment missing: %s", path)
		}
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
