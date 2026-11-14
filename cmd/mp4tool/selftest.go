package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"mp4tool/internal/export"
	"mp4tool/internal/fixture"
	"mp4tool/internal/index"
	"mp4tool/internal/sample"
	"mp4tool/internal/timeline"
)

func cmdSelftest(args []string) int {
	fs := newFlagSet("selftest")
	dir := fs.String("dir", "", "fixture directory (temp dir if empty)")
	if fs.Parse(args) != nil {
		return exitUsage
	}
	return runSelftest(*dir)
}

func runSelftest(dir string) int {
	if dir == "" {
		tmp, err := os.MkdirTemp("", "mp4tool-selftest")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return exitFile
		}
		defer os.RemoveAll(tmp)
		dir = tmp
	}
	if _, err := fixture.GenerateAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, "error: generate fixtures:", err)
		return exitFile
	}
	failed := 0
	check := func(name string, err error) {
		if err != nil {
			failed++
			fmt.Printf("FAIL %s: %v\n", name, err)
		} else {
			fmt.Printf("ok %s\n", name)
		}
	}
	check("inspect_normal", selftestInspectNormal(dir))
	check("seek_normal_landing", selftestSeekNormal(dir))
	check("hash_head", selftestHashHead(dir))
	check("elst_offset", selftestElst(dir))
	check("wrap_restore_export", selftestWrap(dir))
	check("gomp4_alignment", selftestGoMP4(dir))
	check("corrupt_rejected", selftestCorrupt(dir))
	check("front_moov", selftestFront(dir))
	if failed > 0 {
		fmt.Printf("selftest: %d check(s) failed\n", failed)
		return exitSelftest
	}
	fmt.Println("selftest: all checks passed")
	return exitOK
}

func expectEq(got, want interface{}, what string) error {
	if got != want {
		return fmt.Errorf("%s: got %v, want %v", what, got, want)
	}
	return nil
}

func selftestInspectNormal(dir string) error {
	p, err := load(filepath.Join(dir, "normal.mp4"), false)
	if err != nil {
		return err
	}
	defer p.close()
	for _, e := range []struct {
		got, want interface{}
		what      string
	}{
		{p.moov.BoxCount, 312, "boxes"},
		{len(p.moov.Traks), 2, "traks"},
		{p.moov.Timescale, uint32(90000), "timescale"},
		{timeline.TicksToMs(int64(p.moov.Duration), p.moov.Timescale), int64(24000), "duration_ms"},
		{p.moov.Offset, uint64(48213), "moov_offset"},
		{len(p.trak.Table.Sizes), 300, "samples"},
		{len(p.trak.Table.Stss), 25, "keyframes"},
		{len(p.trak.Elst), 1, "elst_segments"},
	} {
		if err := expectEq(e.got, e.want, e.what); err != nil {
			return err
		}
	}
	return nil
}

func selftestSeekNormal(dir string) error {
	p, err := load(filepath.Join(dir, "normal.mp4"), true)
	if err != nil {
		return err
	}
	defer p.close()
	ts := p.trak.Timescale
	f, err := index.Locate(p.frames, timeline.MsToTicks(12500, ts), false)
	if err != nil {
		return err
	}
	for _, e := range []struct {
		got, want interface{}
		what      string
	}{
		{timeline.TicksToMs(f.NormPTS, ts), int64(12480), "landing_pts_ms"},
		{timeline.TicksToMs(f.DTS, ts), int64(12240), "landing_dts_ms"},
		{f.Index, 156, "sample_index"},
		{f.Keyframe, true, "frame_type"},
		{f.PTS, int64(1123200), "raw_pts"},
	} {
		if err := expectEq(e.got, e.want, e.what); err != nil {
			return err
		}
	}
	sum, err := sample.HashSample(p.f, f)
	if err != nil {
		return err
	}
	want := sha256.Sum256(fixture.SampleContent(156, 200))
	return expectEq(sum, hex.EncodeToString(want[:]), "sha256")
}

func selftestHashHead(dir string) error {
	p, err := load(filepath.Join(dir, "normal.mp4"), true)
	if err != nil {
		return err
	}
	defer p.close()
	for i := 0; i < 4; i++ {
		sum, err := sample.HashSample(p.f, p.frames[i])
		if err != nil {
			return err
		}
		size := 100
		if i%12 == 0 {
			size = 200
		}
		want := sha256.Sum256(fixture.SampleContent(i, size))
		if err := expectEq(sum, hex.EncodeToString(want[:]), fmt.Sprintf("hash[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

func selftestElst(dir string) error {
	p, err := load(filepath.Join(dir, "elst.mp4"), true)
	if err != nil {
		return err
	}
	defer p.close()
	ts := p.trak.Timescale
	f, err := index.Locate(p.frames, timeline.MsToTicks(400, ts), false)
	if err != nil {
		return err
	}
	if err := expectEq(f.PTS, int64(72000), "raw_pts"); err != nil {
		return err
	}
	if err := expectEq(timeline.TicksToMs(f.NormPTS, ts), int64(400), "norm_pts_ms"); err != nil {
		return err
	}
	return expectEq(f.Index, 10, "sample_index")
}

func selftestWrap(dir string) error {
	src := filepath.Join(dir, "wrap.mp4")
	p, err := load(src, true)
	if err != nil {
		return err
	}
	defer p.close()
	if err := expectEq(p.wrapEvents, 1, "wrap_events"); err != nil {
		return err
	}
	if err := expectEq(p.frames[0].NormPTS, int64(0), "clamped_norm_pts"); err != nil {
		return err
	}
	if err := expectEq(timeline.TicksToMs(p.frames[4].NormPTS, p.trak.Timescale), int64(300), "norm_pts_ms[4]"); err != nil {
		return err
	}
	out := filepath.Join(dir, "out")
	m, err := export.Export(p.f, src, p.frames, 0, 3000, p.trak.Timescale, out, p.wrapEvents)
	if err != nil {
		return err
	}
	for _, e := range []struct {
		got, want interface{}
		what      string
	}{
		{len(m.Frames), 38, "frames"},
		{m.Bytes, uint64(122880), "bytes"},
		{m.Written, 4, "written"},
		{m.WrapEvents, 1, "wrap_events"},
		{m.Frames[0].PtsMs, int64(0), "first_pts_ms"},
	} {
		if err := expectEq(e.got, e.want, e.what); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(out, "manifest.json")); err != nil {
		return err
	}
	return nil
}

func selftestCorrupt(dir string) error {
	p, err := load(filepath.Join(dir, "corrupt.mp4"), false)
	if err == nil {
		p.close()
		return fmt.Errorf("corrupt file parsed without error")
	}
	return nil
}

func selftestFront(dir string) error {
	p, err := load(filepath.Join(dir, "front.mp4"), false)
	if err != nil {
		return err
	}
	defer p.close()
	if err := expectEq(p.moov.Offset, uint64(24), "moov_offset"); err != nil {
		return err
	}
	if err := expectEq(len(p.trak.Table.Sizes), 30, "samples"); err != nil {
		return err
	}
	return expectEq(len(p.trak.Table.Stss), 3, "keyframes")
}
