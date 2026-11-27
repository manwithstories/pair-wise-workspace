package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	m3u8 "github.com/Eyevinn/hls-m3u8/m3u8"
	"github.com/fashionweek/mediad/internal/media"
	"github.com/fashionweek/mediad/internal/mp4"
)

// rejectOne indexes a corrupt sample and returns its structured media error.
// A nil error means the sample was wrongly accepted, which fails verification.
func rejectOne(path string) (*media.Error, error) {
	f, err := openFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, ierr := mp4.IndexFile(f); ierr != nil {
		me, ok := media.As(ierr)
		if !ok {
			return nil, ierr
		}
		if me.Path == "" {
			me.Path = path
		}
		return me, nil
	}
	return media.New(media.PhaseVerify, media.KindDigest, path, -1,
		"corrupt sample was unexpectedly accepted"), nil
}

// firstPacketKeyframe reports whether the segment's first video packet is a
// keyframe. A standalone fMP4 segment cannot be probed without its
// initialization section, so the EXT-X-MAP init bytes and the segment bytes are
// concatenated into a temporary file that ffprobe can demux as a whole.
func firstPacketKeyframe(segPath string, mp *m3u8.Map) bool {
	if mp == nil {
		return false
	}
	initPath := filepath.Join(filepath.Dir(segPath), filepath.Base(mp.URI))
	combined, err := combineForProbe(initPath, segPath)
	if err != nil {
		return false
	}
	defer os.Remove(combined)

	out, err := exec.Command("ffprobe", "-v", "error",
		"-select_streams", "v:0", "-show_entries", "packet=flags",
		"-of", "csv=p=0", combined).Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// The first demuxed packet belongs to the segment and must be an IDR.
		return strings.Contains(line, "K")
	}
	return false
}

// combineForProbe streams init and segment into one temp file for ffprobe.
func combineForProbe(initPath, segPath string) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(segPath), ".probe-*.mp4")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	for _, p := range []string{initPath, segPath} {
		data, err := os.ReadFile(p)
		if err != nil {
			os.Remove(tmp.Name())
			return "", err
		}
		if _, err := tmp.Write(data); err != nil {
			os.Remove(tmp.Name())
			return "", err
		}
	}
	return tmp.Name(), nil
}
