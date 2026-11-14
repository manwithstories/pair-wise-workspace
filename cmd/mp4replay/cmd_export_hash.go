package main

import (
	"fmt"

	"mp4replay/internal/framehash"
	"mp4replay/internal/mp4atom"
	"mp4replay/internal/remux"
	"mp4replay/internal/report"
	"mp4replay/internal/seekidx"
)

func init() {
	commands["export"] = cmdExport
	commands["hash"] = cmdHash
	commands["verify"] = cmdVerify
}

// resolveLanding returns the nearest sync sample index at or before atMs.
func resolveLanding(tr *mp4atom.Track, atMs int64) (*seekidx.Landing, error) {
	idx := seekidx.Build(tr)
	return idx.Locate(atMs)
}

func cmdExport(args []string) (int, error) {
	fs := flagSet("export")
	fileP := fs.String("file", "", "input mp4")
	atMs := fs.Int64("at-ms", 0, "start presentation time in ms")
	frames := fs.Int("frames", 0, "number of video frames to export")
	outP := fs.String("out", "", "output clip mp4")
	if err := fs.Parse(args); err != nil {
		return report.ExitArgs, err
	}
	if *fileP == "" || *outP == "" || *frames <= 0 {
		return report.ExitArgs, report.NewError(report.ExitArgs, "export",
			"requires --file, --out and positive --frames")
	}
	m, err := mp4atom.Open(*fileP)
	if err != nil {
		return 0, err
	}
	defer m.Close()
	vtr := m.VideoTrack()
	if vtr == nil {
		return report.ExitBoxInvalid, report.NewError(report.ExitBoxInvalid,
			"export", "no video track")
	}
	land, err := resolveLanding(vtr, *atMs)
	if err != nil {
		return 0, err
	}
	start := land.Key.SampleIndex
	end := start + *frames
	if end > len(vtr.Samples) {
		end = len(vtr.Samples)
	}
	spans := []remux.Span{{Track: vtr, Start: start, End: end}}
	if atr := m.AudioTrack(); atr != nil {
		aStart, aEnd := audioRange(atr, vtr, start, end)
		if aEnd > aStart {
			spans = append(spans, remux.Span{
				Track: atr, Start: aStart, End: aEnd,
			})
		}
	}
	if err := remux.Export(m, spans, *outP); err != nil {
		return 0, err
	}
	fmt.Printf("exported samples [%d..%d) -> %s\n", start, end, *outP)
	return report.ExitOK, nil
}

// audioRange maps a video sample interval onto audio samples by PTS overlap.
func audioRange(atr, vtr *mp4atom.Track, vStart, vEnd int) (int, int) {
	if len(vtr.Timeline.PresentMs) == 0 || len(atr.Timeline.PresentMs) == 0 {
		return 0, 0
	}
	startMs := vtr.Timeline.PresentMs[vStart]
	var endMs int64
	if vEnd < len(vtr.Timeline.PresentMs) {
		endMs = vtr.Timeline.PresentMs[vEnd]
	} else {
		endMs = vtr.Timeline.DurationMs
	}
	aPts := atr.Timeline.PresentMs
	aStart := lowerBound(aPts, startMs)
	aEnd := lowerBound(aPts, endMs)
	return aStart, aEnd
}

func lowerBound(xs []int64, target int64) int {
	lo, hi := 0, len(xs)
	for lo < hi {
		mid := (lo + hi) / 2
		if xs[mid] < target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

func cmdHash(args []string) (int, error) {
	fs := flagSet("hash")
	fileP := fs.String("file", "", "input mp4")
	rangeP := fs.String("range", "", "sample range start-end")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return report.ExitArgs, err
	}
	if *fileP == "" {
		return report.ExitArgs, report.NewError(report.ExitArgs, "hash",
			"requires --file")
	}
	m, err := mp4atom.Open(*fileP)
	if err != nil {
		return 0, err
	}
	defer m.Close()
	tr := m.VideoTrack()
	if tr == nil {
		return report.ExitBoxInvalid, report.NewError(report.ExitBoxInvalid,
			"hash", "no video track")
	}
	start, end := 0, len(tr.Samples)
	if *rangeP != "" {
		if start, end, err = parseRange(*rangeP); err != nil {
			return report.ExitArgs, err
		}
	}
	digests, root, total, err := framehash.HashRange(m, tr, start, end)
	if err != nil {
		return 0, err
	}
	res := report.HashResult{
		Frames: len(digests), FirstSha256: digests[0].Sha,
		RootSha256: root, Bytes: total,
	}
	if *asJSON {
		b, _ := report.Marshal(res)
		fmt.Println(string(b))
	} else {
		fmt.Printf("frames=%d first=%s root=%s bytes=%d\n",
			res.Frames, res.FirstSha256, res.RootSha256, res.Bytes)
	}
	return report.ExitOK, nil
}

func cmdVerify(args []string) (int, error) {
	fs := flagSet("verify")
	srcP := fs.String("source", "", "source mp4")
	clipP := fs.String("clip", "", "exported clip mp4")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return report.ExitArgs, err
	}
	if *srcP == "" || *clipP == "" {
		return report.ExitArgs, report.NewError(report.ExitArgs, "verify",
			"requires --source and --clip")
	}
	src, err := mp4atom.Open(*srcP)
	if err != nil {
		return 0, err
	}
	defer src.Close()
	clip, err := mp4atom.Open(*clipP)
	if err != nil {
		return 0, err
	}
	defer clip.Close()
	srcTr := src.VideoTrack()
	clipTr := clip.VideoTrack()
	if srcTr == nil || clipTr == nil {
		return report.ExitBoxInvalid, report.NewError(report.ExitBoxInvalid,
			"verify", "missing video track")
	}
	count := len(clipTr.Samples)
	srcStart, err := framehash.FindSourceStart(src, clip, srcTr, clipTr)
	if err != nil {
		return 0, err
	}
	if srcStart < 0 {
		// first frame may itself be corrupt; align via sample-size pattern
		srcStart = framehash.FindSourceStartBySize(srcTr, clipTr)
	}
	if srcStart < 0 {
		res := report.VerifyResult{Matched: 0, Mismatched: count,
			Status: "unaligned"}
		b, _ := report.Marshal(res)
		fmt.Println(string(b))
		return report.ExitHashMismatch, nil
	}
	matched, mismatched, err := framehash.CompareRange(
		src, clip, srcTr, clipTr, srcStart, 0, count)
	if err != nil {
		return 0, err
	}
	status := "ok"
	code := report.ExitOK
	if mismatched != 0 {
		status = "mismatch"
		code = report.ExitHashMismatch
	}
	res := report.VerifyResult{
		Matched: matched, Mismatched: mismatched, Status: status,
	}
	if *asJSON {
		b, _ := report.Marshal(res)
		fmt.Println(string(b))
	} else {
		fmt.Printf("matched=%d mismatched=%d status=%s\n",
			matched, mismatched, status)
	}
	return code, nil
}
