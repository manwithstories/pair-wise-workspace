package tsslice

import (
	"bufio"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Input is one ordered source segment file.
type Input struct {
	Path      string
	GapBefore int // number of missing sequence slots before this file
}

// OutSegment describes one sliced output segment.
type OutSegment struct {
	Seq        int
	File       string
	DurationMs int
	CRC        uint32
}

// Gap marks missing sequence slots at an output position.
type Gap struct {
	Seq   int // first missing sequence
	Count int
}

// Result summarizes a slicing run.
type Result struct {
	Segments     []OutSegment
	Gaps         []Gap
	RebaseEvents int
	TotalMs      int64
}

type gopPoint struct {
	packet int64
	pts    int64
}

type cut struct {
	start, end int64 // global packet range [start, end)
	durMs      int
}

// Slice re-chunks the ordered input stream at keyframe boundaries closest to
// targetMs, writes segments atomically into outDir, and returns their meta.
func Slice(inputs []Input, outDir string, targetMs int) (*Result, error) {
	if targetMs <= 0 {
		return nil, fmt.Errorf("tsslice: target duration must be positive")
	}
	gops, bounds, endPTS, total, gapAtPacket, err := scanInputs(inputs)
	if err != nil {
		return nil, err
	}
	if len(gops) == 0 {
		return nil, fmt.Errorf("tsslice: no keyframes found in input stream")
	}
	cuts, rebase := planCuts(gops, bounds, endPTS, total, targetMs)
	cuts = splitOverlong(cuts, targetMs)

	var gapList []Gap
	for at, n := range gapAtPacket {
		gapList = append(gapList, Gap{Seq: int(at), Count: n})
	}
	sort.Slice(gapList, func(i, j int) bool { return gapList[i].Seq < gapList[j].Seq })

	seqs := make([]int, len(cuts))
	var gaps []Gap
	seq := 0
	gi := 0
	for i, c := range cuts {
		for gi < len(gapList) && c.start >= int64(gapList[gi].Seq) {
			gaps = append(gaps, Gap{Seq: seq, Count: gapList[gi].Count})
			seq += gapList[gi].Count
			gi++
		}
		seqs[i] = seq
		seq++
	}
	res := &Result{Gaps: gaps, RebaseEvents: rebase}
	if err := writeCuts(inputs, cuts, seqs, outDir, res); err != nil {
		return nil, err
	}
	for _, s := range res.Segments {
		res.TotalMs += int64(s.DurationMs)
	}
	return res, nil
}

// fileBoundary marks the start of one input file and the wall-clock hole
// (missing content, ms) between it and its predecessor. A hole far outside
// the cadence indicates a PTS base reset rather than lost content.
type fileBoundary struct {
	packet int64
	holeMs int
	gap    bool // ingest declared missing sequence slots here
}

// scanInputs collects GOP start points (packet index + PTS), per-file PTS
// boundaries, the stream end PTS, total packet count, and gap positions.
func scanInputs(inputs []Input) ([]gopPoint, []fileBoundary, int64, int64, map[int64]int, error) {
	var gops []gopPoint
	var bounds []fileBoundary
	var lastPTS int64
	var total int64
	gapAt := map[int64]int{}
	prevLastPTS := int64(-1)
	for _, in := range inputs {
		f, err := os.Open(in.Path)
		if err != nil {
			return nil, nil, 0, 0, nil, err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, nil, 0, 0, nil, err
		}
		base := total
		if in.GapBefore > 0 {
			gapAt[base] = in.GapBefore
		}
		fileFirstPTS := int64(-1)
		fileLastPTS := int64(-1)
		scanErr := Scan(f, func(info Info) error {
			if info.Kind == KindVideo && info.HasPTS {
				if fileFirstPTS < 0 {
					fileFirstPTS = info.PTS
				}
				fileLastPTS = info.PTS
				lastPTS = info.PTS
				if info.Keyframe {
					gops = append(gops, gopPoint{packet: base + info.Index, pts: info.PTS})
				}
			}
			return nil
		})
		f.Close()
		if scanErr != nil {
			return nil, nil, 0, 0, nil, fmt.Errorf("tsslice: %s: %w", in.Path, scanErr)
		}
		if fileFirstPTS >= 0 && prevLastPTS >= 0 {
			hole := int((fileFirstPTS - prevLastPTS - 3600) / 90) // 3600 ticks = 40ms frame
			bounds = append(bounds, fileBoundary{packet: base, holeMs: hole, gap: in.GapBefore > 0})
		}
		if fileLastPTS >= 0 {
			prevLastPTS = fileLastPTS
		}
		total += st.Size() / PacketSize
	}
	return gops, bounds, lastPTS + 3600, total, gapAt, nil // +40ms: end of last frame
}

// planCuts groups GOPs into cuts of at least targetMs. Intervals spanning
// file boundaries are adjusted by the measured hole: lost wall-clock time
// is subtracted, while a PTS base reset (jump beyond 2s) re-anchors the
// interval to the GOP cadence so EXTINF stays non-negative and bounded.
func planCuts(gops []gopPoint, bounds []fileBoundary, endPTS, total int64, targetMs int) ([]cut, int) {
	var cuts []cut
	rebase := 0
	start := gops[0]
	// Learn the encoder's GOP cadence as the smallest of the first few
	// positive intervals: holes and forward jumps only inflate intervals,
	// backward resets make them negative, so the minimum is the cadence.
	cadence := 0
	for i := 1; i < len(gops) && i <= 8; i++ {
		if d := int((gops[i].pts - gops[i-1].pts) / 90); d > 0 && (cadence == 0 || d < cadence) {
			cadence = d
		}
	}
	if cadence == 0 {
		cadence = targetMs
	}
	acc := 0
	bi := 0
	adjust := func(d int, uptoPacket int64) int {
		rebased := false
		for bi < len(bounds) && bounds[bi].packet <= uptoPacket {
			b := bounds[bi]
			h := b.holeMs
			switch {
			case b.gap:
				// Declared ingest gap: subtract the measured hole even
				// when several consecutive segments were lost.
				d -= h
			case h < -100 || h > 2000:
				rebase++
				rebased = true
			default:
				d -= h
			}
			bi++
		}
		if rebased {
			return cadence
		}
		if d <= 0 {
			rebase++
			return cadence
		}
		// A clean interval is a whole multiple of the GOP cadence;
		// anything off by more than 2s is a PTS discontinuity.
		k := (d + cadence/2) / cadence
		if k < 1 {
			k = 1
		}
		if dk := d - k*cadence; dk > 2000 || dk < -2000 {
			rebase++
			return cadence
		}
		return d
	}
	for i := 1; i < len(gops); i++ {
		d := int((gops[i].pts - gops[i-1].pts) / 90)
		for bi < len(bounds) && bounds[bi].packet <= gops[i-1].packet {
			bi++
		}
		d = adjust(d, gops[i].packet)
		if d < 1 {
			d = 1
		}
		acc += d
		if acc >= targetMs {
			cuts = append(cuts, cut{start: start.packet, end: gops[i].packet, durMs: acc})
			start = gops[i]
			acc = 0
		}
	}
	tail := int((endPTS - start.pts) / 90)
	tail = adjust(tail, total)
	if tail <= 0 || tail > cadence+2000 {
		tail = cadence
	}
	if tail <= 0 {
		tail = 1
	}
	cuts = append(cuts, cut{start: start.packet, end: total, durMs: tail})
	return cuts, rebase
}

// splitOverlong splits cuts longer than twice the target into two non-zero
// halves so no segment exceeds 2x target and none is zero-length.
func splitOverlong(cuts []cut, targetMs int) []cut {
	var out []cut
	for _, c := range cuts {
		if c.durMs > 2*targetMs && c.end-c.start >= 2 {
			mid := c.start + (c.end-c.start)/2
			half := c.durMs / 2
			out = append(out, cut{c.start, mid, half}, cut{mid, c.end, c.durMs - half})
		} else {
			out = append(out, c)
		}
	}
	return out
}

// writeCuts streams the inputs once, copying packet ranges into atomically
// renamed output files while computing CRC32.
func writeCuts(inputs []Input, cuts []cut, seqs []int, outDir string, res *Result) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	cur := -1
	var out *os.File
	var bw *bufio.Writer
	var hash = crc32.NewIEEE()
	var crc uint32
	closeOut := func() error {
		if out == nil {
			return nil
		}
		if err := bw.Flush(); err != nil {
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		tmp := out.Name()
		final := filepath.Join(outDir, fmt.Sprintf("seg_%06d.ts", seqs[cur]))
		if err := os.Rename(tmp, final); err != nil {
			return err
		}
		res.Segments = append(res.Segments, OutSegment{
			Seq:        seqs[cur],
			File:       filepath.Base(final),
			DurationMs: cuts[cur].durMs,
			CRC:        crc,
		})
		out = nil
		return nil
	}

	var global int64
	pkt := make([]byte, PacketSize)
	for _, in := range inputs {
		f, err := os.Open(in.Path)
		if err != nil {
			return err
		}
		br := bufio.NewReaderSize(f, 1<<20)
		for {
			_, err := io.ReadFull(br, pkt)
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			if err != nil {
				f.Close()
				return err
			}
			for cur+1 < len(cuts) && global == cuts[cur+1].start {
				if err := closeOut(); err != nil {
					f.Close()
					return err
				}
				cur++
				tmp := filepath.Join(outDir, fmt.Sprintf(".seg_%06d.tmp", seqs[cur]))
				out, err = os.Create(tmp)
				if err != nil {
					f.Close()
					return err
				}
				bw = bufio.NewWriterSize(out, 1<<20)
				hash.Reset()
			}
			if out != nil && global < cuts[cur].end {
				if _, err := bw.Write(pkt); err != nil {
					f.Close()
					return err
				}
				hash.Write(pkt)
				crc = hash.Sum32()
			}
			global++
		}
		f.Close()
	}
	return closeOut()
}
