package ingest

import (
	"io"
	"strings"

	"mediamux/media"
)

// VTTParser 增量解析 WebVTT 流，不将完整输入载入内存。
type VTTParser struct {
	br     *blockReader
	fr     media.FrameRate
	header bool
	seq    int
}

// NewVTTParser 构造 WebVTT 解析器；maxRecord 为单条字幕字节上限。
func NewVTTParser(r io.Reader, fr media.FrameRate, maxRecord int) *VTTParser {
	return &VTTParser{br: newBlockReader(r, maxRecord), fr: fr}
}

// Next 返回下一条字幕事件。io.EOF 表示结束；其余错误为 *media.Error，
// 可恢复错误在跳过坏记录后可继续调用。缺少 WEBVTT 头为不可恢复错误。
func (p *VTTParser) Next() (*Event, error) {
	for {
		blk, err := p.br.next()
		if err != nil {
			return nil, err
		}
		if !p.header {
			p.header = true
			if !strings.HasPrefix(blk.lines[0], "WEBVTT") {
				return nil, media.New(media.StageParse, "sub", blk.off, false,
					"缺少 WEBVTT 头: %q", blk.lines[0])
			}
			continue
		}
		first := blk.lines[0]
		if strings.HasPrefix(first, "NOTE") || first == "STYLE" || first == "REGION" {
			continue // 注释与样式块不属于字幕事件
		}
		return p.parseCue(blk)
	}
}

func (p *VTTParser) parseCue(blk *block) (*Event, error) {
	i := 0
	if !strings.Contains(blk.lines[0], "-->") {
		i = 1 // 首行是可选 cue 标识符
	}
	if len(blk.lines) <= i {
		return nil, media.New(media.StageParse, "sub", blk.off, true,
			"VTT 块缺少时间轴")
	}
	start, end, err := ParseCueTimes(blk.lines[i], p.fr, blk.off)
	if err != nil {
		return nil, err
	}
	if end < start {
		return nil, media.New(media.StageParse, "sub", blk.off, true,
			"字幕结束时间早于开始时间")
	}
	p.seq++
	return &Event{
		Seq:     p.seq,
		Start:   start,
		End:     end,
		Text:    strings.Join(blk.lines[i+1:], "\n"),
		Offset:  blk.off,
		ByteLen: int64(blk.raw),
	}, nil
}
