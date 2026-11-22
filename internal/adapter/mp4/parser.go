package mp4

import (
	"bytes"
	"context"

	"mediacheck/internal/adapter"
	"mediacheck/internal/model"
)

// Parser 是 fMP4 容器适配器，版本 1.0.0。
type Parser struct{}

func (Parser) Family() string  { return "mp4" }
func (Parser) Version() string { return "1.0.0" }

// 本适配器声明支持的 codec 白名单；其它 codec 需新版本适配器接入。
var supportedCodecs = []string{"ipcm", "avc1"}

func New() *Parser { return &Parser{} }

// SupportedCodecsList 暴露本适配器声明支持的 codec，供注册表接入。
func SupportedCodecsList() []string { return supportedCodecs }

func (p *Parser) Parse(ctx context.Context, in adapter.ParseInput) (*adapter.ParseResult, error) {
	if err := adapter.CheckInputLimits(in); err != nil {
		return nil, err
	}
	out := &adapter.ParseResult{}
	if len(in.InitSegment) > 0 {
		tracks, boxes, err := parseInit(in.InitSegment, in.MaxBoxSize)
		if err != nil {
			return nil, err
		}
		out.Tracks = tracks
		out.Boxes = boxes
	}
	trackByID := map[uint32]model.TrackMeta{}
	for _, tm := range out.Tracks {
		trackByID[tm.TrackID] = tm
	}
	for _, raw := range in.Fragments {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		frag, err := parseFragment(raw, in.MaxBoxSize, trackByID)
		if err != nil {
			return nil, err
		}
		out.Fragments = append(out.Fragments, frag)
	}
	return out, nil
}

// parseInit 遍历初始化段，抽取每轨 tkhd/mdhd/hdlr/stsd 元数据。
func parseInit(data []byte, maxBox int64) ([]model.TrackMeta, []model.BoxSummary, error) {
	r := bytes.NewReader(data)
	var boxes []model.BoxSummary
	tracks := map[uint32]*model.TrackMeta{}
	var order []uint32
	var trackStack []uint32
	curTrack := func() uint32 {
		if len(trackStack) == 0 {
			return 0
		}
		return trackStack[len(trackStack)-1]
	}
	err := walkBoxes(r, int64(len(data)), 0, func(rb rawBox, depth int) error {
		summarize(&boxes)(rb, depth)
		typ := rb.info.Type.String()
		switch typ {
		case "trak":
			trackStack = append(trackStack, 0)
		case "tkhd":
			id, ok := parseTkhd(rb.payload)
			if !ok {
				return &model.MediaError{Category: model.ErrTruncatedBox, Offset: int64(rb.info.Offset), Message: "tkhd payload truncated"}
			}
			if len(trackStack) > 0 {
				trackStack[len(trackStack)-1] = id
			}
			if _, exists := tracks[id]; !exists {
				tracks[id] = &model.TrackMeta{TrackID: id}
				order = append(order, id)
			}
		case "mdhd":
			ts, ok := parseMdhdTimescale(rb.payload)
			if !ok {
				return &model.MediaError{Category: model.ErrTruncatedBox, Offset: int64(rb.info.Offset), Message: "mdhd payload truncated"}
			}
			if t := tracks[curTrack()]; t != nil {
				t.Timescale = ts
			}
		case "hdlr":
			if t := tracks[curTrack()]; t != nil {
				t.Codec = hdlrHint(rb.payload)
			}
		case "stsd":
			codec, rate, layout, depth2, color, ok := parseStsd(rb.payload)
			if !ok {
				return &model.MediaError{Category: model.ErrTruncatedBox, Offset: int64(rb.info.Offset), Message: "stsd payload truncated"}
			}
			t := tracks[curTrack()]
			if t == nil {
				return nil
			}
			t.Codec = codec
			t.SampleRate = rate
			t.Layout = layout
			t.BitDepth = depth2
			t.Color = color
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	res := make([]model.TrackMeta, 0, len(order))
	for _, id := range order {
		res = append(res, *tracks[id])
	}
	return res, boxes, nil
}
