package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mediasync/internal/media"
	"mediasync/internal/mp4"
	"mediasync/internal/subtitle"
)

// inspectLimits cap how much a single file may contribute to one run.
const (
	maxSubtitleFile = subtitle.MaxSubtitleBytes
	maxMP4File      = mp4.MaxFileBytes
)

// runInspect parses caller-supplied subtitle and MP4 files. Unlike the demo it
// never marks damage as expected, so any recoverable defect yields exit code 2
// and an unreadable input yields exit code 1.
func runInspect(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		subs = fs.String("subtitle", "", "comma-separated .srt/.vtt files to parse")
		mp4s = fs.String("mp4", "", "comma-separated .mp4 files to index")
	)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "mediasync: unexpected argument %q\n", fs.Arg(0))
		return 1
	}
	if strings.TrimSpace(*subs) == "" && strings.TrimSpace(*mp4s) == "" {
		fmt.Fprintln(stderr, "mediasync: inspect requires --subtitle or --mp4")
		return 1
	}

	issues := 0
	recoverable := 0
	fatal := false
	report := func(is *media.Issue) {
		issues++
		if is.Fatal {
			fatal = true
			fmt.Fprintf(stdout, "FATAL   %s\n", is.Error())
			return
		}
		recoverable++
		fmt.Fprintf(stdout, "recover %s\n", is.Error())
	}

	for _, path := range splitList(*subs) {
		data, err := readLimited(path, maxSubtitleFile)
		if err != nil {
			fmt.Fprintf(stderr, "mediasync: %s: %v\n", path, err)
			return 1
		}
		res := subtitle.Detect(data, filepath.Base(path))
		for _, is := range res.Issues {
			report(is)
		}
		fmt.Fprintf(stdout, "%s: format=%s cues=%d lines=%d bytes=%d\n",
			path, res.Format, len(res.Cues), res.Lines, res.Bytes)
		if res.Fatal() {
			fatal = true
		}
	}

	for _, path := range splitList(*mp4s) {
		data, err := readLimited(path, maxMP4File)
		if err != nil {
			fmt.Fprintf(stderr, "mediasync: %s: %v\n", path, err)
			return 1
		}
		tracks := mp4.Index(data, filepath.Base(path))
		for _, tr := range tracks {
			for _, is := range tr.Issues {
				report(is)
			}
			fmt.Fprintf(stdout, "%s: track=%d handler=%s samples=%d keyframes=%d rebuilt=%v\n",
				path, tr.TrackID, orDash(tr.Handler), len(tr.Samples), len(tr.Keyframes), tr.RebuiltKeys)
			if tr.Fatal() {
				fatal = true
			}
		}
	}

	fmt.Fprintf(stdout, "inspect: issues=%d recoverable=%d fatal=%v\n", issues, recoverable, fatal)
	switch {
	case fatal:
		return 1
	case recoverable > 0:
		return 2
	default:
		return 0
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// readLimited reads at most limit bytes and rejects path traversal and
// oversized inputs before allocating the buffer.
func readLimited(path string, limit int) ([]byte, error) {
	if strings.ContainsRune(path, 0) {
		return nil, errors.New("path contains NUL")
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return nil, fmt.Errorf("path %q contains a '..' component", path)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, errors.New("is a directory")
	}
	if info.Size() > int64(limit) {
		return nil, fmt.Errorf("file is %d bytes, limit is %d", info.Size(), limit)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, 0, info.Size())
	out, err := io.Copy(bytesWriter{&buf}, io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if out > int64(limit) {
		return nil, fmt.Errorf("file exceeded %d bytes while reading", limit)
	}
	return buf, nil
}

type bytesWriter struct{ buf *[]byte }

func (w bytesWriter) Write(p []byte) (int, error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
