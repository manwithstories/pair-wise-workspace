package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"strings"
)

const (
	fixtureW, fixtureH = 320, 180
	fixtureFPS         = 25
	fixtureFrames      = 40
	ticksPerFrame      = 3600 // 90000/25
	wrapModulus        = int64(1) << 33
)

// genFrame 生成确定性合成帧：平滑正弦色彩场 + 移动色块。
func genFrame(track, seq int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, fixtureW, fixtureH))
	for y := 0; y < fixtureH; y++ {
		for x := 0; x < fixtureW; x++ {
			r := uint8(128 + 90*math.Sin(float64(x+seq*4+track*30)*0.03))
			g := uint8(128 + 90*math.Sin(float64(y+seq*2+track*50)*0.035))
			b := uint8(128 + 90*math.Sin(float64(x+y+seq*3+track*10)*0.02))
			img.SetNRGBA(x, y, color.NRGBA{r, g, b, 255})
		}
	}
	bx := (seq*7 + track*60) % (fixtureW - 40)
	by := (seq*3 + track*30) % (fixtureH - 40)
	for y := by; y < by+40; y++ {
		for x := bx; x < bx+40; x++ {
			img.SetNRGBA(x, y, color.NRGBA{uint8(200 - track*40), uint8(60 + track*50), 40, 255})
		}
	}
	return img
}

type anchorFixture struct {
	pts   int64 // 清单中的原始（可能已回绕）PTS
	refMs float64
}

type trackFixture struct {
	id       string
	priority int
	modulus  int64
	basePTS  int64
	corrupt  int // 破坏该序号帧内容（-1 无）
	skip     map[int64]bool
	anchors  []anchorFixture
}

func (tf *trackFixture) rawPTS(i int64) int64 {
	pts := tf.basePTS + i*ticksPerFrame
	if tf.modulus > 0 {
		pts %= tf.modulus
	}
	return pts
}

// genCaseFixtures 在 dir 下生成指定样例的清单与源帧，返回清单路径与源帧根目录。
func genCaseFixtures(caseName, dir string) (string, string, error) {
	srcRoot := filepath.Join(dir, "src")
	tracks := []*trackFixture{
		{id: "camA", priority: 1, corrupt: -1},
		{id: "camB", priority: 2, corrupt: -1},
	}
	switch caseName {
	case "normal", "truncated":
	case "wrap_drift":
		camA := tracks[0]
		camA.modulus = wrapModulus
		camA.basePTS = wrapModulus - 20*ticksPerFrame // 中途回绕
		// 锚点把本地大 PTS 映射到机房参考时钟（0 起的统一时间线），
		// 线性漂移 5e-6 ms/tick；第 4 个锚点异常（+4s），须被拒绝。
		for k, i := range []int64{0, 8, 16, 24, 32, 39} {
			u := camA.basePTS + i*ticksPerFrame
			d := float64(u - camA.basePTS)
			ref := d*1000/90000 + 5e-6*d
			if k == 3 {
				ref += 4000
			}
			camA.anchors = append(camA.anchors, anchorFixture{pts: u % wrapModulus, refMs: ref})
		}
	case "badhash":
		tracks[0].corrupt = 7
	case "gap":
		tracks[0].skip = map[int64]bool{10: true, 11: true, 12: true}
	default:
		return "", "", fmt.Errorf("unknown demo case %q", caseName)
	}
	var header strings.Builder
	header.WriteString(`{"type":"header","version":1,"timebase":{"num":1,"den":90000},"tracks":[`)
	for i, tf := range tracks {
		if i > 0 {
			header.WriteByte(',')
		}
		fmt.Fprintf(&header, `{"id":%q,"priority":%d,"fps":%d,"modulus":%d}`,
			tf.id, tf.priority, fixtureFPS, tf.modulus)
	}
	header.WriteString("]}")
	var body []string
	for ti, tf := range tracks {
		lines, err := writeTrack(srcRoot, tf, ti)
		if err != nil {
			return "", "", err
		}
		body = append(body, lines...)
		for _, a := range tf.anchors {
			body = append(body, fmt.Sprintf(`{"type":"anchor","track":%q,"pts":%d,"ref_ms":%v}`,
				tf.id, a.pts, a.refMs))
		}
	}
	manifestPath := filepath.Join(dir, "manifest.jsonl")
	content := header.String() + "\n" + strings.Join(body, "\n") + "\n"
	if err := os.WriteFile(manifestPath, []byte(content), 0o644); err != nil {
		return "", "", err
	}
	return manifestPath, srcRoot, nil
}

// writeTrack 写出一轨源帧 JPEG 并返回清单帧记录行。
func writeTrack(srcRoot string, tf *trackFixture, trackIdx int) ([]string, error) {
	if err := os.MkdirAll(filepath.Join(srcRoot, tf.id), 0o755); err != nil {
		return nil, err
	}
	var lines []string
	for i := int64(0); i < fixtureFrames; i++ {
		if tf.skip[int64(i)] {
			continue
		}
		img := genFrame(trackIdx, int(i))
		var sb strings.Builder
		if err := jpeg.Encode(&sb, img, &jpeg.Options{Quality: 92}); err != nil {
			return nil, err
		}
		data := []byte(sb.String())
		rel := fmt.Sprintf("%s/%04d.jpg", tf.id, i)
		if err := os.WriteFile(filepath.Join(srcRoot, rel), data, 0o644); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		if int(i) == tf.corrupt {
			data[0] ^= 0xff // 破坏内容但保留清单原哈希
			if err := os.WriteFile(filepath.Join(srcRoot, rel), data, 0o644); err != nil {
				return nil, err
			}
		}
		lines = append(lines, fmt.Sprintf(
			`{"type":"frame","track":%q,"seq":%d,"pts":%d,"dur":%d,"sha256":%q,"file":%q}`,
			tf.id, i, tf.rawPTS(i), ticksPerFrame, hex.EncodeToString(sum[:]), rel))
	}
	return lines, nil
}
