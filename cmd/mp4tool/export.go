package main

import (
	"fmt"
	"os"
	"path/filepath"

	"mp4tool/internal/export"
)

func cmdExport(args []string) int {
	fs := newFlagSet("export")
	file := fs.String("file", "", "input mp4 file")
	fromMs := fs.Int64("from-ms", 0, "range start (presentation ms)")
	toMs := fs.Int64("to-ms", -1, "range end (presentation ms)")
	out := fs.String("out", "", "output directory")
	if fs.Parse(args) != nil || *file == "" || *toMs < *fromMs || *out == "" {
		return exitUsage
	}
	srcAbs, err1 := filepath.Abs(filepath.Dir(*file))
	outAbs, err2 := filepath.Abs(*out)
	if err1 != nil || err2 != nil || srcAbs == outAbs {
		fmt.Fprintln(os.Stderr, "error: output directory must differ from source directory")
		return exitUsage
	}
	p, err := load(*file, true)
	if err != nil {
		return fail(err)
	}
	defer p.close()
	m, err := export.Export(p.f, *file, p.frames, *fromMs, *toMs, p.trak.Timescale, *out, p.wrapEvents)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("wrap_events=%d written=%d frames=%d bytes=%d\n", m.WrapEvents, m.Written, len(m.Frames), m.Bytes)
	for i := 0; i < 3 && i < len(m.Frames); i++ {
		fmt.Printf("sha256[%d]=%s\n", i, m.Frames[i].SHA256)
	}
	return exitOK
}
