// Package manifest parses and validates media manifests. Each line
// describes one representation: camera, bitrate tier, media timescale
// and a local path confined to the manifest's base directory.
package manifest

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"mediaroute/internal/model"
)

// Entry is one validated manifest line.
type Entry struct {
	Camera    string
	Bitrate   uint32 // kbps tier label
	Timescale uint32 // declared media timescale (Hz)
	Path      string // absolute path, confined to the base directory
}

// ID returns the representation identity of the entry.
func (e Entry) ID() model.RepresentationID {
	return model.RepresentationID{Camera: e.Camera, Bitrate: e.Bitrate}
}

const (
	maxBitrate   = 10_000_000 // kbps
	maxTimescale = 10_000_000 // Hz
)

// Parse reads manifest lines of the form:
//
//	camera=<id> bitrate=<kbps> timescale=<hz> path=<relative-file>
//
// Blank lines and lines starting with '#' are ignored. It rejects
// out-of-base paths, duplicate representations and invalid timebases.
func Parse(r io.Reader, baseDir string) ([]Entry, error) {
	base, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, model.New(model.CodeIO, "manifest base", err)
	}
	seen := map[model.RepresentationID]int{}
	var entries []Entry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		e, err := parseLine(line, base)
		if err != nil {
			return nil, model.Newf(model.CodeManifest, "manifest",
				"line %d: %v", lineNo, err)
		}
		id := e.ID()
		if prev, dup := seen[id]; dup {
			return nil, model.Newf(model.CodeManifest, "manifest",
				"line %d: duplicate representation %s (first on line %d)",
				lineNo, id, prev)
		}
		seen[id] = lineNo
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, model.New(model.CodeIO, "manifest read", err)
	}
	if len(entries) == 0 {
		return nil, model.Newf(model.CodeManifest, "manifest", "no entries")
	}
	return entries, nil
}

func parseLine(line, base string) (Entry, error) {
	var e Entry
	var relPath string
	for _, field := range strings.Fields(line) {
		kv := strings.SplitN(field, "=", 2)
		if len(kv) != 2 {
			return e, fmt.Errorf("malformed field %q (want key=value)", field)
		}
		switch kv[0] {
		case "camera":
			e.Camera = kv[1]
		case "bitrate":
			v, err := strconv.ParseUint(kv[1], 10, 32)
			if err != nil || v == 0 || v > maxBitrate {
				return e, fmt.Errorf("invalid bitrate %q", kv[1])
			}
			e.Bitrate = uint32(v)
		case "timescale":
			v, err := strconv.ParseUint(kv[1], 10, 32)
			if err != nil || v == 0 || v > maxTimescale {
				return e, fmt.Errorf("invalid timescale %q", kv[1])
			}
			e.Timescale = uint32(v)
		case "path":
			relPath = kv[1]
		default:
			return e, fmt.Errorf("unknown key %q", kv[0])
		}
	}
	if e.Camera == "" || strings.ContainsAny(e.Camera, "/\\") {
		return e, fmt.Errorf("invalid camera id %q", e.Camera)
	}
	if e.Bitrate == 0 {
		return e, fmt.Errorf("missing bitrate")
	}
	if e.Timescale == 0 {
		return e, fmt.Errorf("missing timescale")
	}
	p, err := confine(base, relPath)
	if err != nil {
		return e, err
	}
	e.Path = p
	return e, nil
}

// confine resolves rel inside base and rejects anything escaping it.
func confine(base, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("missing path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute path %q not allowed", rel)
	}
	p := filepath.Join(base, filepath.Clean(rel))
	if p != base && !strings.HasPrefix(p, base+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes base directory", rel)
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("path %q: %v", rel, err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("path %q is not a regular file", rel)
	}
	return p, nil
}
