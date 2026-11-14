package fixtures

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/transform"

	"subtool/internal/subtitle"
)

type Summary struct {
	OutDir        string
	Cues          int
	Files         int
	OverlapGroups int
}

func Generate(outDir string, count int, seed int64) (Summary, error) {
	if count <= 0 {
		return Summary{}, fmt.Errorf("count must be positive")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return Summary{}, err
	}
	summary := Summary{OutDir: outDir, Cues: count}
	write := func(name string, data []byte) error {
		if err := os.WriteFile(filepath.Join(outDir, name), data, 0o644); err != nil {
			return err
		}
		summary.Files++
		return nil
	}
	stress := buildTrack(subtitle.FormatSRT, "stress", count, 0, 40, seed)
	stressData, err := subtitle.Write(stress, subtitle.FormatSRT)
	if err != nil {
		return summary, err
	}
	summary.OverlapGroups = subtitle.OverlapGroupCount(stress.Cues)
	if err := write("stress_50k.srt", stressData); err != nil {
		return summary, err
	}
	mixed := buildTrack(subtitle.FormatSRT, "mixed", count, 0, 40, seed)
	mixedData, err := subtitle.Write(mixed, subtitle.FormatSRT)
	if err != nil {
		return summary, err
	}
	gb, err := encode(simplifiedchinese.GB18030.NewEncoder(), mixedData)
	if err != nil {
		return summary, err
	}
	if err := write("mixed_gb18030.srt", gb); err != nil {
		return summary, err
	}
	bomData := append([]byte{0xef, 0xbb, 0xbf}, stressData...)
	if err := write("mixed_utf8_bom.srt", bomData); err != nil {
		return summary, err
	}
	extra := min(count, 800)
	ep01 := buildTrack(subtitle.FormatASS, "ep01", extra, 1250, 0, seed)
	ep01.Cues[0].StartMS = 1250
	ep01.Cues[0].EndMS = 3750
	for i := 1; i < len(ep01.Cues); i++ {
		ep01.Cues[i].StartMS = 5000 + int64(i-1)*1000
		ep01.Cues[i].EndMS = ep01.Cues[i].StartMS + 800
	}
	subtitle.Normalize(ep01)
	assData, err := subtitle.Write(ep01, subtitle.FormatASS)
	if err != nil {
		return summary, err
	}
	if err := write("ep01.ass", assData); err != nil {
		return summary, err
	}
	srtData, err := subtitle.Write(ep01, subtitle.FormatSRT)
	if err != nil {
		return summary, err
	}
	if err := write("ep01.srt", srtData); err != nil {
		return summary, err
	}
	if err := writeEncoded(write, "mixed_big5.vtt", traditional(extra, seed), traditionalchinese.Big5.NewEncoder(), subtitle.FormatVTT); err != nil {
		return summary, err
	}
	japaneseTrack := buildJapanese(extra, seed)
	if err := writeEncoded(write, "legacy_shiftjis.ass", japaneseTrack, japanese.ShiftJIS.NewEncoder(), subtitle.FormatASS); err != nil {
		return summary, err
	}
	drawing := drawTrack()
	drawingData, err := subtitle.Write(drawing, subtitle.FormatASS)
	if err != nil {
		return summary, err
	}
	if err := write("ass_drawing.ass", drawingData); err != nil {
		return summary, err
	}
	invalid := []byte("1\n00:00:01,000 --> 00:00:01,000\nzero length\n\n2\n00:00:03,000 --> 00:00:02,000\nreversed\n\n3\n00:00:04,000 --> 00:00:05,000\nvalid\n")
	if err := write("invalid.srt", invalid); err != nil {
		return summary, err
	}
	malformed := map[string][]byte{
		"truncated.srt":         []byte("1\n00:00:00,000 --> 00:00:01,000\nfirst\n\n2\n00:00:02,000 -->"),
		"missing_events.ass":    []byte("[Script Info]\nScriptType: v4.00+\n"),
		"short_dialogue.ass":    []byte("[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:02.00\n"),
		"timeline_overflow.vtt": []byte("WEBVTT\n\n1\n100:00:00.000 --> 100:00:01.000\noverflow\n"),
		"low_confidence.srt":    []byte("1\n00:00:00,000 --> 00:00:01,000\n\xff\xfe\x80\x81\n"),
	}
	for name, data := range malformed {
		if err := write(name, data); err != nil {
			return summary, err
		}
	}
	return summary, nil
}

func writeEncoded(write func(string, []byte) error, name string, track *subtitle.Track,
	enc *encoding.Encoder, format subtitle.Format) error {
	data, err := subtitle.Write(track, format)
	if err != nil {
		return err
	}
	encoded, err := encode(enc, data)
	if err != nil {
		return err
	}
	return write(name, encoded)
}

func encode(enc *encoding.Encoder, data []byte) ([]byte, error) {
	out, _, err := transform.Bytes(enc, data)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func buildTrack(format subtitle.Format, prefix string, count int, base int64, overlapEvery int, seed int64) *subtitle.Track {
	cues := make([]subtitle.Cue, 0, count)
	for i := 0; i < count; i++ {
		start := base + int64(i)*1000 + 3
		if overlapEvery > 0 && i > 0 && (i-1)%overlapEvery == 0 {
			start -= 600
		}
		end := start + 807
		if overlapEvery > 0 && i%overlapEvery == 0 && i+1 < count {
			end = start + 1200
		}
		cues = append(cues, subtitle.Cue{Index: i + 1, StartMS: start, EndMS: end,
			Text: bilingual(prefix, i+1, seed), Style: "Default", Layer: "0"})
	}
	track := &subtitle.Track{Format: format, Cues: cues}
	subtitle.Normalize(track)
	return track
}

func bilingual(prefix string, n int, seed int64) string {
	lines := []string{"这件事必须在天亮前完成。", "我们把证据留在车站。", "他从来没有忘记那天的雨。", "请再确认一次时间码。"}
	index := (n + int(seed%int64(len(lines)))) % len(lines)
	if index < 0 {
		index += len(lines)
	}
	return fmt.Sprintf("%s %d：%s\nLine %d: The timeline must stay exact.", prefix, n, lines[index], n)
}

func traditional(count int, seed int64) *subtitle.Track {
	cues := make([]subtitle.Cue, 0, count)
	for i := 0; i < count; i++ {
		start := int64(i) * 1000
		cues = append(cues, subtitle.Cue{Index: i + 1, StartMS: start, EndMS: start + 800,
			Text: fmt.Sprintf("第%d句：這個故事從這裡開始。\nLine %d: Keep every cue. seed=%d", i+1, i+1, seed)})
	}
	track := &subtitle.Track{Format: subtitle.FormatVTT, Cues: cues}
	subtitle.Normalize(track)
	return track
}

func buildJapanese(count int, seed int64) *subtitle.Track {
	cues := make([]subtitle.Cue, 0, count)
	for i := 0; i < count; i++ {
		start := int64(i) * 1000
		cues = append(cues, subtitle.Cue{Index: i + 1, StartMS: start, EndMS: start + 800,
			Text: fmt.Sprintf("第%d話：これは物語の始まりです。\nLine %d: 旧工程 seed=%d", i+1, i+1, seed)})
	}
	track := &subtitle.Track{Format: subtitle.FormatASS, Cues: cues}
	subtitle.Normalize(track)
	return track
}

func drawTrack() *subtitle.Track {
	cues := []subtitle.Cue{
		{Index: 1, StartMS: 1000, EndMS: 3000, Text: `{\p1}m 0 0 l 100 0 100 100 0 100{\p0}`},
		{Index: 2, StartMS: 4000, EndMS: 6000, Text: "normal dialogue"},
	}
	track := &subtitle.Track{Format: subtitle.FormatASS, Cues: cues}
	subtitle.Normalize(track)
	return track
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
