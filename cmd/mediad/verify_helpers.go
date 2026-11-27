package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/fashionweek/mediad/internal/media"
	"github.com/fashionweek/mediad/internal/mp4"
	m3u8 "github.com/Eyevinn/hls-m3u8/m3u8"
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
// keyframe. The segment is probed together with its EXT-X-MAP init section so
// fMP4 headers resolve correctly.
func firstPacketKeyframe(segPath string, mp *m3u8.Map) bool {
	args := []string{"-v", "error", "-select_streams", "v:0",
		"-show_entries", "packet=flags", "-of", "csv=p=0"}
	if mp != nil {
		initPath := filepath.Join(filepath.Dir(segPath), filepath.Base(mp.URI))
		args = append([]string{"-init", initPath}, args...)
	}
	args = append(args, segPath)
	out, err := exec.Command("ffprobe", args...).Output()
	if err != nil {
		// ffprobe may reject the -init placement; retry probing the segment's
		// moof directly by scanning packets without the map where supported.
		return probeFirstFlag(segPath)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return strings.Contains(line, "K")
	}
	return false
}

// probeFirstFlag is a fallback that reads packet flags without an init path.
func probeFirstFlag(segPath string) bool {
	out, err := exec.Command("ffprobe", "-v", "error",
		"-select_streams", "v:0", "-show_entries", "packet=flags",
		"-of", "csv=p=0", segPath).Output()
	if err != nil {
		return false
	}
	first := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	if len(first) == 0 {
		return false
	}
	return bytes.Contains(first[0], []byte("K"))
}
