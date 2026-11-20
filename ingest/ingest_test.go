package ingest

import (
	"errors"
	"io"
	"strings"
	"testing"

	"mediamux/media"
)

// chunkReader 每次 Read 只返回 1 字节，模拟跨块到达的字幕行。
type chunkReader struct{ s string }

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.s) == 0 {
		return 0, io.EOF
	}
	p[0] = c.s[0]
	c.s = c.s[1:]
	return 1, nil
}

func TestParseTimecodeFrames(t *testing.T) {
	// 25fps：01:00:00:12 = 90012 帧
	got, err := ParseTimecode("01:00:00:12", media.FrameRate25, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := media.FrameRate25.FramesToTicks(90012); got != want {
		t.Fatalf("got %d want %d", got, want)
	}
	// 29.97 非法帧号 30
	if _, err := ParseTimecode("00:00:01:30", media.FrameRate2997, 7); err == nil {
		t.Fatal("frame 30 @29.97 should fail")
	} else if e := media.As(err); e.Offset != 7 || !e.Recoverable {
		t.Fatalf("error model: %+v", e)
	}
	// 日界回绕歧义
	if _, err := ParseTimecode("25:00:00:00", media.FrameRate25, 0); err == nil {
		t.Fatal("HH=25 should fail")
	}
	// 负偏移
	got, err = ParseTimecode("-00:00:10:00", media.FrameRate25, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != -media.FrameRate25.FramesToTicks(250) {
		t.Fatalf("negative = %d", got)
	}
}

func TestParseTimecodeClock(t *testing.T) {
	got, err := ParseTimecode("00:01:02,500", media.FrameRate25, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := media.MillisToTicks(62500); got != want {
		t.Fatalf("got %d want %d", got, want)
	}
	// VTT 短格式 MM:SS.mmm
	got, err = ParseTimecode("01:02.5", media.FrameRate25, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := media.MillisToTicks(62500); got != want {
		t.Fatalf("short form got %d want %d", got, want)
	}
	// 溢出
	if _, err := ParseTimecode("99999999:00:00.000", media.FrameRate25, 3); err == nil {
		t.Fatal("overflow should fail")
	}
	// 分量越界
	if _, err := ParseTimecode("00:61:00.000", media.FrameRate25, 0); err == nil {
		t.Fatal("MM=61 should fail")
	}
}

func TestSRTStream(t *testing.T) {
	// BOM + CRLF + 跨块读取 + 末尾无换行
	data := "\xef\xbb\xbf1\r\n00:00:01,000 --> 00:00:02,000\r\n你好\r\n\r\n" +
		"2\r\n00:00:03,000 --> 00:00:04,500\r\n世界\n第二行\r\n\r\n" +
		"3\r\n00:00:05,000 --> 00:00:06,000\r\n末尾无换行"
	p := NewSRTParser(&chunkReader{data}, media.FrameRate25, 4096)
	var evs []*Event
	for {
		ev, err := p.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
	if len(evs) != 3 {
		t.Fatalf("events = %d", len(evs))
	}
	if evs[0].Offset != 3 { // BOM 之后
		t.Fatalf("first offset = %d", evs[0].Offset)
	}
	if evs[0].Start != media.MillisToTicks(1000) || evs[1].End != media.MillisToTicks(4500) {
		t.Fatalf("times: %d %d", evs[0].Start, evs[1].End)
	}
	if evs[1].Text != "世界\n第二行" || evs[2].Text != "末尾无换行" {
		t.Fatalf("text: %q %q", evs[1].Text, evs[2].Text)
	}
}

func TestSRTRecoverable(t *testing.T) {
	// 坏块（缺时间轴）+ 超限块夹在好块之间，均可恢复且偏移可追溯
	long := strings.Repeat("x", 100)
	data := "1\n00:00:01,000 --> 00:00:02,000\n好\n\n" +
		"2\n坏块无时间轴\n\n" +
		"3\n00:00:03,000 --> 00:00:04,000\n" + long + "\n\n" +
		"4\n00:00:05,000 --> 00:00:06,000\n仍有效"
	p := NewSRTParser(strings.NewReader(data), media.FrameRate25, 64)
	var evs []*Event
	var errs []*media.Error
	for {
		ev, err := p.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			e := media.As(err)
			if !e.Recoverable {
				t.Fatalf("should be recoverable: %v", e)
			}
			errs = append(errs, e)
			continue
		}
		evs = append(evs, ev)
	}
	if len(evs) != 2 || evs[0].Seq != 1 || evs[1].Seq != 4 {
		t.Fatalf("events = %+v", evs)
	}
	if len(errs) != 2 {
		t.Fatalf("errors = %d", len(errs))
	}
	// 坏块偏移必须指向其在原始输入中的真实位置
	bad2 := data[len("1\n00:00:01,000 --> 00:00:02,000\n好\n\n"):]
	if errs[0].Offset != int64(len(data)-len(bad2)) {
		t.Fatalf("bad block offset = %d", errs[0].Offset)
	}
	if !strings.Contains(data[errs[1].Offset:], long) {
		t.Fatalf("overlong offset = %d", errs[1].Offset)
	}
}

func TestVTT(t *testing.T) {
	data := "WEBVTT - 演示\n\nNOTE 注释块\n被跳过\n\n" +
		"cue-1\n00:00:01.000 --> 00:00:02.000 align:start position:0%\n你好\n\n" +
		"00:00:03.000 --> 00:00:04.000\n无标识符\n"
	p := NewVTTParser(strings.NewReader(data), media.FrameRate25, 4096)
	var evs []*Event
	for {
		ev, err := p.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
	if len(evs) != 2 || evs[0].Text != "你好" || evs[1].Text != "无标识符" {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].Start != media.MillisToTicks(1000) {
		t.Fatalf("start = %d", evs[0].Start)
	}
	// 缺少 WEBVTT 头为不可恢复
	p2 := NewVTTParser(strings.NewReader("1\n00:00:01.000 --> 00:00:02.000\nx\n"), media.FrameRate25, 4096)
	if _, err := p2.Next(); err == nil || media.IsRecoverable(err) {
		t.Fatalf("missing header should be unrecoverable: %v", err)
	}
}
