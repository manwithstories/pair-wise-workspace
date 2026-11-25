// Package render 从确定性时间线导出 PNG 帧序列、逐帧来源清单和校验和。
package render

import (
	"bytes"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sort"

	"mediaarchive/internal/archive"
	"mediaarchive/internal/clock"
	"mediaarchive/internal/fault"
	"mediaarchive/internal/ingest"
	"mediaarchive/internal/timeline"
)

// DefaultFrameInterval 为每秒一个可复核帧。
const DefaultFrameInterval clock.Ticks = clock.Ticks(clock.Hz)

// Options 控制离屏渲染和清单参数。
type Options struct {
	Out        string
	Width      int
	Height     int
	Interval   clock.Ticks
	Profile    string
	Seed       int64
	Events     int
	Tracks     int
	ParamsHash string
	StateHash  string
}

// Export 导出全部 PNG 和逐帧来源清单，返回可写入顶层 manifest 的模型及帧序列哈希。
func Export(packets []ingest.Packet, st timeline.State, opt Options) (archive.Manifest, string, error) {
	if opt.Width <= 0 {
		opt.Width = 320
	}
	if opt.Height <= 0 {
		opt.Height = 180
	}
	if opt.Interval <= 0 {
		opt.Interval = DefaultFrameInterval
	}
	if err := os.MkdirAll(filepath.Join(opt.Out, "frames"), 0o755); err != nil {
		return archive.Manifest{}, "", err
	}
	if err := os.MkdirAll(filepath.Join(opt.Out, "manifests"), 0o755); err != nil {
		return archive.Manifest{}, "", err
	}

	trackIDs := sortedTracks(packets)
	trackIndex := make(map[string]int, len(trackIDs))
	for i, t := range trackIDs {
		trackIndex[t] = i
	}
	lastEnd := clock.Ticks(0)
	for _, p := range packets {
		if e := timeline.End(p); e > lastEnd {
			lastEnd = e
		}
	}
	for _, h := range st.Holes {
		if clock.Ticks(h.EndTicks) > lastEnd {
			lastEnd = clock.Ticks(h.EndTicks)
		}
	}
	frameCount := int((int64(lastEnd) + int64(opt.Interval) - 1) / int64(opt.Interval))
	if frameCount == 0 {
		frameCount = 1
	}

	lateKeys := map[string]bool{}
	for _, le := range st.LateEvents {
		lateKeys[le.Track+"#"+uintToString(le.ExtSeq)] = true
	}

	mf := archive.Manifest{
		Version:            archive.ManifestVersion,
		Profile:            opt.Profile,
		Seed:               opt.Seed,
		Events:             opt.Events,
		Tracks:             opt.Tracks,
		FrameIntervalTicks: int64(opt.Interval),
		ParamsHash:         opt.ParamsHash,
		StateHash:          opt.StateHash,
		Holes:              append([]fault.Hole(nil), st.Holes...),
		Conflicts:          append([]fault.Conflict(nil), st.Conflicts...),
		LateEvents:         append([]fault.LateImpact(nil), st.LateEvents...),
		Frames:             make([]archive.FrameManifest, 0, frameCount),
	}
	pngHashes := make([]string, 0, frameCount)

	for fi := 0; fi < frameCount; fi++ {
		fs := clock.Ticks(int64(fi) * int64(opt.Interval))
		fe := fs + opt.Interval
		fm := archive.FrameManifest{
			Frame: fi, PTSTicks: int64(fs), IntervalTicks: int64(opt.Interval),
			PNGPath: filepath.ToSlash(filepath.Join("frames", frameName(fi, ".png"))),
		}

		img := image.NewRGBA(image.Rect(0, 0, opt.Width, opt.Height))
		drawBackground(img)
		laneH := opt.Height / max(1, len(trackIDs))
		if laneH < 24 {
			laneH = 24
		}

		for _, p := range packets {
			pe := timeline.End(p)
			if p.Start >= fe || pe <= fs {
				continue
			}
			flags := []string(nil)
			if lateKeys[p.Identity()] {
				flags = append(flags, "late_replayed")
			}
			fm.Sources = append(fm.Sources, archive.ProvenanceFromPacket(p, flags...))
			drawPacket(img, p, fs, fe, trackIndex[p.TrackID], laneH, opt.Width)
		}
		for _, h := range st.Holes {
			hs, he := clock.Ticks(h.StartTicks), clock.Ticks(h.EndTicks)
			if hs >= fe || he <= fs {
				continue
			}
			fm.HoleTicks = append(fm.HoleTicks, h)
			drawHole(img, hs, he, fs, fe, trackIndex[h.Track], laneH, opt.Width)
		}
		drawFrameChrome(img, fi, frameCount)

		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return archive.Manifest{}, "", err
		}
		pngBytes := buf.Bytes()
		pngHash := BytesSHA256(pngBytes)
		pngHashes = append(pngHashes, pngHash)
		pngPath := filepath.Join(opt.Out, fm.PNGPath)
		if err := archive.AtomicWriteFile(pngPath, pngBytes, 0o644); err != nil {
			return archive.Manifest{}, "", err
		}
		fm.PNGSHA256 = pngHash

		relSources := filepath.ToSlash(filepath.Join("manifests", frameName(fi, ".sources.json")))
		fm.SourceManifestPath = relSources
		if _, err := archive.WriteJSON(filepath.Join(opt.Out, relSources), fm); err != nil {
			return archive.Manifest{}, "", err
		}
		mf.Frames = append(mf.Frames, fm)
	}

	return mf, SequenceHash(pngHashes), nil
}

func sortedTracks(packets []ingest.Packet) []string {
	set := map[string]bool{}
	for _, p := range packets {
		set[p.TrackID] = true
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func xFor(t, fs, fe clock.Ticks, width int) int {
	return int((int64(t-fs) * int64(width)) / int64(fe-fs))
}

func drawBackground(img *image.RGBA) {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 18, G: 22, B: 30, A: 255})
		}
	}
}

func drawPacket(img *image.RGBA, p ingest.Packet, fs, fe clock.Ticks, lane, laneH, width int) {
	x0 := xFor(maxTicks(p.Start, fs), fs, fe, width)
	x1 := xFor(minTicks(timeline.End(p), fe), fs, fe, width)
	if x1 <= x0 {
		x1 = x0 + 1
	}
	y0 := lane*laneH + 8
	y1 := y0 + laneH - 16
	if y1 <= y0 {
		y1 = y0 + 2
	}
	base := trackColor(p.TrackID)
	if len(p.Hash) >= 2 {
		if v, err := hex.DecodeString(p.Hash[:2]); err == nil && len(v) == 1 {
			base.G = uint8(80 + int(v[0])%120)
		}
	}
	fillRect(img, x0, y0, x1, y1, base)
}

func drawHole(img *image.RGBA, hs, he, fs, fe clock.Ticks, lane, laneH, width int) {
	x0 := xFor(maxTicks(hs, fs), fs, fe, width)
	x1 := xFor(minTicks(he, fe), fs, fe, width)
	if x1 <= x0 {
		x1 = x0 + 2
	}
	y0 := lane*laneH + 10
	y1 := y0 + laneH - 18
	fillRect(img, x0, y0, x1, y1, color.RGBA{R: 220, G: 70, B: 70, A: 180})
}

func drawFrameChrome(img *image.RGBA, frame, total int) {
	b := img.Bounds()
	for x := 0; x < b.Dx(); x++ {
		img.SetRGBA(x, 0, color.RGBA{R: 90, G: 170, B: 255, A: 255})
		img.SetRGBA(x, 1, color.RGBA{R: 90, G: 170, B: 255, A: 255})
	}
	// 左上角画帧号二进制条，保证不依赖字体也能肉眼/像素复核序号。
	for bit := 0; bit < 16; bit++ {
		if frame&(1<<bit) != 0 {
			fillRect(img, 4+bit*4, 5, 6+bit*4, 11, color.RGBA{R: 120, G: 230, B: 160, A: 255})
		}
	}
	_ = total
}

func fillRect(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	b := img.Bounds()
	if x0 < b.Min.X {
		x0 = b.Min.X
	}
	if y0 < b.Min.Y {
		y0 = b.Min.Y
	}
	if x1 > b.Max.X {
		x1 = b.Max.X
	}
	if y1 > b.Max.Y {
		y1 = b.Max.Y
	}
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			img.SetRGBA(x, y, c)
		}
	}
}

func trackColor(track string) color.RGBA {
	switch track {
	case "track-1-aac":
		return color.RGBA{R: 70, G: 150, B: 230, A: 255}
	case "track-2-opus":
		return color.RGBA{R: 150, G: 210, B: 90, A: 255}
	default:
		return color.RGBA{R: 190, G: 120, B: 220, A: 255}
	}
}

func maxTicks(a, b clock.Ticks) clock.Ticks {
	if a > b {
		return a
	}
	return b
}

func minTicks(a, b clock.Ticks) clock.Ticks {
	if a < b {
		return a
	}
	return b
}

func uintToString(v uint64) string {
	// 小型十进制转换，避免仅为一处拼接引入 strconv 到渲染热路径。
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func frameName(i int, ext string) string {
	s := uintToString(uint64(i))
	for len(s) < 6 {
		s = "0" + s
	}
	return "frame-" + s + ext
}
