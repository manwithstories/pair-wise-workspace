package ingest

import (
	"bufio"
	"io"
	"strconv"
	"strings"

	"mediamux/media"
)

// Event 是统一时间事件：字幕区间、原文与输入偏移。
type Event struct {
	Seq     int
	Start   media.Ticks
	End     media.Ticks
	Text    string
	Offset  int64 // 记录首字节在原始输入中的偏移
	ByteLen int64 // 记录原始字节数（含块尾空行）
}

// block 是一条原始字幕块（空行分隔）。
type block struct {
	off   int64
	raw   int
	lines []string
}

// blockReader 增量读取空行分隔的块，处理 CRLF、BOM、
// 跨块长行与末尾无换行；单块超过 maxRec 字节时报告可恢复错误。
type blockReader struct {
	r      *bufio.Reader
	off    int64
	eof    bool
	maxRec int
}

func newBlockReader(r io.Reader, maxRec int) *blockReader {
	br := bufio.NewReaderSize(r, 4096)
	off := int64(0)
	// 跳过 UTF-8 BOM，偏移从 BOM 之后起算
	if bs, err := br.Peek(3); err == nil && bs[0] == 0xEF && bs[1] == 0xBB && bs[2] == 0xBF {
		_, _ = br.Discard(3)
		off = 3
	}
	return &blockReader{r: br, off: off, maxRec: maxRec}
}

// next 读取下一块；io.EOF 表示输入结束。
func (b *blockReader) next() (*block, error) {
	if b.eof {
		return nil, io.EOF
	}
	var blk *block
	over := false
	for {
		frag, err := b.r.ReadSlice('\n')
		isPrefix := err == bufio.ErrBufferFull // 长行分片，尚未到换行
		if len(frag) > 0 {
			if blk == nil {
				blk = &block{off: b.off}
			}
			b.off += int64(len(frag))
			blk.raw += len(frag)
			if blk.raw > b.maxRec {
				over = true
			}
			if !isPrefix {
				line := strings.TrimRight(string(frag), "\r\n")
				if line == "" { // 空行：块结束
					if over {
						return nil, media.New(media.StageParse, "sub", blk.off, true,
							"记录超过单条上限 %d 字节", b.maxRec)
					}
					if len(blk.lines) > 0 {
						return blk, nil
					}
					blk = nil // 块间多余空行，继续找下一块
					continue
				}
				if !over {
					blk.lines = append(blk.lines, line)
				}
			}
		}
		if err == bufio.ErrBufferFull {
			continue // 长行分片，继续读
		}
		if err == io.EOF {
			b.eof = true
			if blk == nil || len(blk.lines) == 0 {
				if blk != nil && over {
					return nil, media.New(media.StageParse, "sub", blk.off, true,
						"记录超过单条上限 %d 字节", b.maxRec)
				}
				return nil, io.EOF
			}
			if over {
				return nil, media.New(media.StageParse, "sub", blk.off, true,
					"记录超过单条上限 %d 字节", b.maxRec)
			}
			return blk, nil // 末尾无换行
		}
		if err != nil {
			return nil, err
		}
	}
}

// SRTParser 增量解析 SRT 流，不将完整输入载入内存。
type SRTParser struct {
	br *blockReader
	fr media.FrameRate
}

// NewSRTParser 构造 SRT 解析器；maxRecord 为单条字幕字节上限。
func NewSRTParser(r io.Reader, fr media.FrameRate, maxRecord int) *SRTParser {
	return &SRTParser{br: newBlockReader(r, maxRecord), fr: fr}
}

// Next 返回下一条字幕事件。io.EOF 表示结束；其余错误为 *media.Error，
// 可恢复错误在跳过坏记录后可继续调用 Next 处理后续有效记录。
func (p *SRTParser) Next() (*Event, error) {
	blk, err := p.br.next()
	if err != nil {
		return nil, err
	}
	return parseSRTBlock(blk, p.fr)
}

func parseSRTBlock(blk *block, fr media.FrameRate) (*Event, error) {
	if len(blk.lines) < 2 {
		return nil, media.New(media.StageParse, "sub", blk.off, true,
			"SRT 块行数不足（%d 行）", len(blk.lines))
	}
	seq, err := strconv.Atoi(strings.TrimSpace(blk.lines[0]))
	if err != nil {
		return nil, media.New(media.StageParse, "sub", blk.off, true,
			"SRT 序号非法: %q", blk.lines[0])
	}
	start, end, err := ParseCueTimes(blk.lines[1], fr, blk.off)
	if err != nil {
		return nil, err
	}
	if end < start {
		return nil, media.New(media.StageParse, "sub", blk.off, true,
			"字幕结束时间早于开始时间")
	}
	return &Event{
		Seq:     seq,
		Start:   start,
		End:     end,
		Text:    strings.Join(blk.lines[2:], "\n"),
		Offset:  blk.off,
		ByteLen: int64(blk.raw),
	}, nil
}

// ParseCueTimes 解析 "start --> end" 时间轴行（SRT/VTT 共用；
// VTT 结束时间后的 cue 设置按空白截断忽略）。
func ParseCueTimes(line string, fr media.FrameRate, off int64) (media.Ticks, media.Ticks, error) {
	parts := strings.SplitN(line, "-->", 2)
	if len(parts) != 2 {
		return 0, 0, media.New(media.StageParse, "sub", off, true,
			"缺少 --> 时间轴: %q", line)
	}
	start, err := ParseTimecode(strings.TrimSpace(parts[0]), fr, off)
	if err != nil {
		return 0, 0, err
	}
	endField := strings.TrimSpace(parts[1])
	if i := strings.IndexAny(endField, " \t"); i >= 0 {
		endField = endField[:i]
	}
	end, err := ParseTimecode(endField, fr, off)
	if err != nil {
		return 0, 0, err
	}
	return start, end, nil
}
