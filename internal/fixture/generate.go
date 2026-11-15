// Package fixture generates deterministic multi-format, multi-encoding
// sample corpora for selftest and demos. No external files are used.
package fixture

import (
	"bytes"
	"fmt"
	"io"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"

	"subtool/internal/model"
	"subtool/internal/serialize"
)

// Corpus describes the generated fixture set.
type Corpus struct {
	Dir      string
	Cues     []model.Cue // canonical generated cues
	Files    []string    // written file paths
	BadFiles []string    // files intentionally containing bad timecodes
}

var texts = []string{
	"指挥官，<i>轨道电梯</i>已就绪。",
	"{\\an8}双说话人场景：甲先开口。",
	"双说话人场景：<b>乙同时插话</b>。",
	"跨帧率吸附测试，毫秒级对齐。",
	"字幕母版交付，第 N 条。",
	"Inline tag <font color=\"#FF0000\">红色</font> 保留。",
}

// GenCues builds n deterministic cues with occasional controlled
// dual-speaker overlaps and inline tags.
func GenCues(seed int64, n int) []model.Cue {
	rng := rand.New(rand.NewSource(seed))
	cues := make([]model.Cue, 0, n)
	cursor := int64(1000) // ms
	for len(cues) < n {
		dur := 800 + rng.Int63n(2200)
		gap := int64(120 + rng.Int63n(400))
		start := cursor
		end := start + dur
		text := texts[len(cues)%len(texts)]
		cues = append(cues, model.Cue{
			ID:    fmt.Sprintf("%04d", len(cues)+1),
			Start: big.NewRat(start, 1),
			End:   big.NewRat(end, 1),
			Text:  text,
		})
		// Every 16th slot gets a dual-speaker partner overlapping on
		// the same span; layer resolution must keep both.
		if len(cues)%16 == 8 && len(cues) < n {
			cues = append(cues, model.Cue{
				ID:    fmt.Sprintf("%04d", len(cues)+1),
				Start: big.NewRat(start+dur/3, 1),
				End:   big.NewRat(end+300, 1),
				Text:  "（双人重叠轨）" + text,
			})
		}
		cursor = end + gap
	}
	return cues
}

// Generate writes the full fixture corpus into dir and returns it.
func Generate(dir string, seed int64, n int) (*Corpus, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	cues := GenCues(seed, n)
	c := &Corpus{Dir: dir, Cues: cues}
	write := func(name string, content []byte) error {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, content, 0o644); err != nil {
			return err
		}
		c.Files = append(c.Files, p)
		return nil
	}
	srt, _ := serialize.Write(cues, serialize.SRT)
	vtt, _ := serialize.Write(cues, serialize.VTT)
	ass, _ := serialize.Write(cues, serialize.ASS)
	ttml, _ := serialize.Write(cues, serialize.TTML)

	gb, err := encode(simplifiedchinese.GB18030.NewEncoder(), srt)
	if err != nil {
		return nil, err
	}
	u16, err := encode(
		unicode.UTF16(unicode.LittleEndian, unicode.ExpectBOM).NewEncoder(), srt)
	if err != nil {
		return nil, err
	}
	// mixed.srt is GB18030-encoded to exercise sniffing on the demo path.
	for _, f := range []struct {
		name string
		data []byte
	}{
		{"mixed.srt", gb},
		{"sample_utf16le.srt", u16},
		{"sample_bom.vtt", append([]byte{0xEF, 0xBB, 0xBF}, vtt...)},
		{"sample.ass", []byte(ass)},
		{"sample.ttml", []byte(ttml)},
	} {
		if err := write(f.name, f.data); err != nil {
			return nil, err
		}
	}
	// bad.srt: >24h, zero-length, end<start, malformed rows + one good.
	bad := "1\n25:00:00,000 --> 25:00:01,000\n超过24小时\n\n" +
		"2\n00:00:05,000 --> 00:00:05,000\n零长条目\n\n" +
		"3\n00:00:10,000 --> 00:00:09,000\n结束早于开始\n\n" +
		"4\nnot-a-timecode --> 00:00:11,000\n坏时码\n\n" +
		"5\n00:00:12,000 --> 00:00:13,500\n正常条目\n\n"
	if err := write("bad.srt", []byte(bad)); err != nil {
		return nil, err
	}
	c.BadFiles = append(c.BadFiles, filepath.Join(dir, "bad.srt"))
	return c, nil
}

// encode runs s through a transformer and returns the bytes.
func encode(t transform.Transformer, s string) ([]byte, error) {
	var buf bytes.Buffer
	w := transform.NewWriter(&buf, t)
	if _, err := io.WriteString(w, s); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
