package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"mp4tool/internal/timeline"
)

type batchLine struct {
	File         string `json:"file"`
	Boxes        int    `json:"boxes,omitempty"`
	Traks        int    `json:"traks,omitempty"`
	Timescale    uint32 `json:"timescale,omitempty"`
	DurationMs   int64  `json:"duration_ms,omitempty"`
	MoovOffset   uint64 `json:"moov_offset,omitempty"`
	Samples      int    `json:"samples,omitempty"`
	Keyframes    int    `json:"keyframes,omitempty"`
	ElstSegments int    `json:"elst_segments,omitempty"`
	Error        string `json:"error,omitempty"`
}

func cmdBatch(args []string) int {
	fs := newFlagSet("batch")
	dir := fs.String("dir", "", "directory to scan")
	jobs := fs.Int("jobs", 4, "fixed worker count")
	if fs.Parse(args) != nil || *dir == "" || *jobs < 1 {
		return exitUsage
	}
	matches, err := filepath.Glob(filepath.Join(*dir, "*.mp4"))
	if err != nil {
		return fail(err)
	}
	sort.Strings(matches)
	lines := make([]batchLine, len(matches))
	sem := make(chan struct{}, *jobs)
	var wg sync.WaitGroup
	for i, path := range matches {
		wg.Add(1)
		go func(i int, path string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			lines[i] = inspectOne(path)
		}(i, path)
	}
	wg.Wait()
	failed := 0
	for _, l := range lines {
		if l.Error != "" {
			failed++
		}
		data, _ := json.Marshal(l)
		fmt.Println(string(data))
	}
	fmt.Fprintf(os.Stderr, "total=%d failed=%d\n", len(lines), failed)
	if failed == len(lines) && len(lines) > 0 {
		return exitFile
	}
	return exitOK
}

func inspectOne(path string) batchLine {
	l := batchLine{File: filepath.Base(path)}
	p, err := load(path, false)
	if err != nil {
		l.Error = err.Error()
		return l
	}
	defer p.close()
	l.Boxes = p.moov.BoxCount
	l.Traks = len(p.moov.Traks)
	l.Timescale = p.moov.Timescale
	l.DurationMs = timeline.TicksToMs(int64(p.moov.Duration), p.moov.Timescale)
	l.MoovOffset = p.moov.Offset
	if p.trak != nil {
		l.Samples = len(p.trak.Table.Sizes)
		l.Keyframes = len(p.trak.Table.Stss)
		if l.Keyframes == 0 {
			l.Keyframes = l.Samples
		}
		l.ElstSegments = len(p.trak.Elst)
	}
	return l
}
