package fixture

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"segctl/internal/tsslice"
)

// Inject describes how many faults of each kind to inject per rendition.
type Inject struct {
	Loss    int
	Dup     int
	Reorder int
	Rebase  int
}

// ParseInject parses "loss=3,dup=2,reorder=5,rebase=1".
func ParseInject(s string) (Inject, error) {
	var inj Inject
	if s == "" {
		return inj, nil
	}
	for _, kv := range splitComma(s) {
		key, valStr, ok := strings.Cut(kv, "=")
		if !ok {
			return inj, fmt.Errorf("fixture: bad inject pair %q", kv)
		}
		val, err := strconv.Atoi(valStr)
		if err != nil {
			return inj, fmt.Errorf("fixture: bad inject count in %q", kv)
		}
		if val < 0 {
			return inj, fmt.Errorf("fixture: negative inject count in %q", kv)
		}
		switch key {
		case "loss":
			inj.Loss = val
		case "dup":
			inj.Dup = val
		case "reorder":
			inj.Reorder = val
		case "rebase":
			inj.Rebase = val
		default:
			return inj, fmt.Errorf("fixture: unknown inject kind %q", key)
		}
	}
	return inj, nil
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}

// RenditionSpec describes one ladder rung.
type RenditionSpec struct {
	Name        string `json:"rendition"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	BitrateKbps int    `json:"bitrate_kbps"`
	GopMs       int    `json:"gop_ms"`
}

// DefaultRenditions is the four-rung ladder; the 1080p encoder uses a 5s GOP.
func DefaultRenditions() []RenditionSpec {
	return []RenditionSpec{
		{Name: "240p", Width: 426, Height: 240, BitrateKbps: 400, GopMs: 2000},
		{Name: "480p", Width: 854, Height: 480, BitrateKbps: 800, GopMs: 2000},
		{Name: "720p", Width: 1280, Height: 720, BitrateKbps: 2500, GopMs: 2000},
		{Name: "1080p", Width: 1920, Height: 1080, BitrateKbps: 5800, GopMs: 5000},
	}
}

// Arrival is one network arrival event referencing a file on disk.
type Arrival struct {
	Seq  int64  `json:"seq"`
	File string `json:"file"`
}

type arrivalLog struct {
	Rendition string    `json:"rendition"`
	Arrivals  []Arrival `json:"arrivals"`
}

// SourceMeta is written next to the raw segments so later stages know the
// encoder targets without re-deriving them.
type SourceMeta struct {
	Rendition   string `json:"rendition"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	BitrateKbps int    `json:"bitrate_kbps"`
	GopMs       int    `json:"gop_ms"`
	SegmentMs   int    `json:"segment_ms"`
	Segments    int    `json:"segments"`
	RebaseAt    []int  `json:"rebase_at"`
}

// ReadSourceMeta loads the raw-side source metadata.
func ReadSourceMeta(path string) (*SourceMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m SourceMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("fixture: parse %s: %w", path, err)
	}
	return &m, nil
}

// ReadArrivals loads the arrival log of a raw rendition directory.
func ReadArrivals(path string) ([]Arrival, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var log arrivalLog
	if err := json.Unmarshal(data, &log); err != nil {
		return nil, fmt.Errorf("fixture: parse %s: %w", path, err)
	}
	return log.Arrivals, nil
}

// Generate synthesizes all four renditions concurrently.
func Generate(outDir string, minutes int, seed int64, inj Inject) error {
	if minutes < 1 {
		return fmt.Errorf("fixture: minutes must be >= 1")
	}
	specs := DefaultRenditions()
	// The four encoders share the stadium uplink, so backhaul dropouts
	// knock out the same time window on every rendition.
	sharedLoss := pickSet(rand.New(rand.NewSource(seed)), minutes*60*1000/rawSegMs, inj.Loss, nil)
	var g errgroup.Group
	for i, spec := range specs {
		g.Go(func() error {
			return genRendition(outDir, spec, minutes, seed+int64(i)*7919, inj, sharedLoss)
		})
	}
	return g.Wait()
}

const (
	frameMs    = 40
	rawSegMs   = 2000
	ticksPerMs = 90
)

func genRendition(root string, spec RenditionSpec, minutes int, seed int64, inj Inject, loss map[int]bool) error {
	dir := filepath.Join(root, spec.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	numSegs := minutes * 60 * 1000 / rawSegMs
	rng := rand.New(rand.NewSource(seed))

	dup := pickSet(rng, numSegs, inj.Dup, loss)
	reorder := pickSet(rng, numSegs, inj.Reorder, loss)
	rebase := pickSet(rng, numSegs, inj.Rebase, loss)
	delete(rebase, 0) // rebasing the very first segment is a no-op

	videoPerFrame := spec.BitrateKbps * 1000 / 8 * 9 / 10 * frameMs / 1000
	audioPerFrame := spec.BitrateKbps * 1000 / 8 / 10 * frameMs / 1000
	video := make([]byte, videoPerFrame)
	audio := make([]byte, audioPerFrame)
	copy(audio, []byte{0xFF, 0xF1, 0x50, 0x80, 0, 0x1F, 0xFC}) // ADTS-ish header

	ptsBase := int64(0)
	for s := 0; s < numSegs; s++ {
		if rebase[s] {
			// Encoder reset: PTS base jumps back near zero. Early in the
			// program a reset near zero would be indistinguishable from
			// normal cadence, so force a forward jump instead; either way
			// the discontinuity exceeds the 2s rebase threshold.
			resetMs := int64(rng.Intn(1000))
			curMs := int64(s * rawSegMs)
			if resetMs > curMs-2000 && resetMs < curMs+3000 {
				resetMs = curMs + 3000 + int64(rng.Intn(3000))
			}
			ptsBase = resetMs*ticksPerMs - int64(s*rawSegMs)*ticksPerMs
		}
		if loss[s] {
			continue
		}
		name := fmt.Sprintf("seg_%06d.ts", s)
		if err := writeRawSegment(filepath.Join(dir, name), spec, s, ptsBase, video, audio); err != nil {
			return err
		}
	}
	if err := writeArrivals(dir, spec.Name, numSegs, loss, dup, reorder, rng); err != nil {
		return err
	}
	return writeSourceMeta(dir, spec, numSegs, rebase)
}

// writeRawSegment muxes one raw segment covering [s*rawSegMs, (s+1)*rawSegMs).
func writeRawSegment(path string, spec RenditionSpec, segIdx int, ptsBase int64, video, audio []byte) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	mux := tsslice.NewMuxer(f)
	if err := mux.WritePATPMT(); err != nil {
		f.Close()
		return err
	}
	start := segIdx * rawSegMs
	for t := start; t < start+rawSegMs; t += frameMs {
		pts := ptsBase + int64(t)*ticksPerMs
		keyframe := t%spec.GopMs == 0
		if keyframe {
			if err := mux.WritePATPMT(); err != nil {
				f.Close()
				return err
			}
		}
		nal := byte(0x41)
		if keyframe {
			nal = 0x65
		}
		copy(video, []byte{0x00, 0x00, 0x00, 0x01, nal})
		if err := mux.WritePES(tsslice.PIDVideo, 0xE0, pts, keyframe, video); err != nil {
			f.Close()
			return err
		}
		if err := mux.WritePES(tsslice.PIDAudio, 0xC0, pts, false, audio); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// writeArrivals builds the arrival log with loss/dup/reorder injected.
func writeArrivals(dir, rendition string, numSegs int, loss, dup, reorder map[int]bool, rng *rand.Rand) error {
	var arrivals []Arrival
	for s := 0; s < numSegs; s++ {
		if loss[s] {
			continue
		}
		name := fmt.Sprintf("seg_%06d.ts", s)
		arrivals = append(arrivals, Arrival{Seq: int64(s), File: name})
		if dup[s] {
			if s%2 == 0 {
				// Conflicting retransmission: same sequence, different bytes.
				alt := fmt.Sprintf("seg_%06d.alt.ts", s)
				if err := corruptCopy(filepath.Join(dir, name), filepath.Join(dir, alt)); err != nil {
					return err
				}
				arrivals = append(arrivals, Arrival{Seq: int64(s), File: alt})
			} else {
				arrivals = append(arrivals, Arrival{Seq: int64(s), File: name})
			}
		}
	}
	// Reorder: swap arrivals two slots apart. A swap is rolled back when
	// it would push any arrival past the retransmission window (a loss
	// between the pair inflates the effective sequence distance).
	idxs := make([]int, 0, len(reorder))
	for s := range reorder {
		idxs = append(idxs, s)
	}
	sort.Ints(idxs)
	for _, s := range idxs {
		pos := -1
		for i, a := range arrivals {
			if a.Seq == int64(s) {
				pos = i
				break
			}
		}
		if pos >= 0 && pos+2 < len(arrivals) {
			arrivals[pos], arrivals[pos+2] = arrivals[pos+2], arrivals[pos]
			if expiredArrivals(arrivals, 3) > 0 {
				arrivals[pos], arrivals[pos+2] = arrivals[pos+2], arrivals[pos]
			}
		}
	}
	log := arrivalLog{Rendition: rendition, Arrivals: arrivals}
	data, err := json.MarshalIndent(log, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "arrival.json"), data, 0o644)
}

// expiredArrivals simulates the ingest reorderer over an arrival sequence
// and counts how many unique arrivals would fall outside the window.
func expiredArrivals(arrivals []Arrival, window int) int {
	next := int64(0)
	seen := map[int64]bool{}
	buffered := map[int64]bool{}
	expired := 0
	drain := func() {
		for {
			if buffered[next] {
				delete(buffered, next)
				next++
				continue
			}
			top := int64(-1)
			for s := range buffered {
				if top < 0 || s < top {
					top = s
				}
			}
			if top >= 0 && top-next >= int64(window) {
				next++ // gap declared
				continue
			}
			return
		}
	}
	for _, a := range arrivals {
		if seen[a.Seq] {
			continue
		}
		if a.Seq < next {
			expired++
			continue
		}
		seen[a.Seq] = true
		buffered[a.Seq] = true
		drain()
	}
	return expired
}

func writeSourceMeta(dir string, spec RenditionSpec, numSegs int, rebase map[int]bool) error {
	var pts []int
	for s := range rebase {
		pts = append(pts, s)
	}
	sort.Ints(pts)
	m := SourceMeta{
		Rendition:   spec.Name,
		Width:       spec.Width,
		Height:      spec.Height,
		BitrateKbps: spec.BitrateKbps,
		GopMs:       spec.GopMs,
		SegmentMs:   rawSegMs,
		Segments:    numSegs,
		RebaseAt:    pts,
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "source_meta.json"), data, 0o644)
}

// corruptCopy copies a segment flipping one byte so its CRC differs.
func corruptCopy(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	// Flip a payload byte inside a packet, never the 0x47 sync byte, so the
	// container stays parseable and only the content CRC changes.
	if len(data) > 200 {
		off := (len(data)/2/188)*188 + 100
		data[off] ^= 0xFF
	}
	return os.WriteFile(dst, data, 0o644)
}

// pickSet chooses n distinct segment indices, excluding any index in excl.
func pickSet(rng *rand.Rand, numSegs, n int, excl map[int]bool) map[int]bool {
	set := map[int]bool{}
	if n <= 0 {
		return set
	}
	for _, i := range rng.Perm(numSegs) {
		if len(set) >= n {
			break
		}
		if excl[i] {
			continue
		}
		set[i] = true
	}
	return set
}
