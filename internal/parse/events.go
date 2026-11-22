package parse

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/abema/go-mp4"
)

// Event 是一条跨轨定时元数据，统一承载 emsg/ID3/SEI/字幕/章节。
type Event struct {
	TrackID   uint32 `json:"track_id"`
	Kind      string `json:"kind"` // emsg | id3 | sei | subtitle | chapter
	StartNs   int64  `json:"start_ns"`
	EndNs     int64  `json:"end_ns"`
	RawTick   uint64 `json:"raw_tick"`    // 映射前原始时间戳
	ErrNs     int64  `json:"err_ns"`      // 时钟量化误差
	Source    string `json:"source"`      // 来源盒/包标识
	Text      string `json:"text"`
	SchemeURI string `json:"scheme_uri,omitempty"`
	EventID   uint32 `json:"event_id,omitempty"`
}

// 事件类型常量。
const (
	EvEmsg     = "emsg"
	EvID3      = "id3"
	EvSEI      = "sei"
	EvSubtitle = "subtitle"
	EvChapter  = "chapter"
)

// ParseEvents 按依赖图顺序在盒树中收集跨轨事件。
// 调用方须保证媒体轨道（提供时钟）已先解析；无法映射的事件保留原始 tick。
func ParseEvents(r io.ReadSeeker, roots []*RawBox, tis []*TrackInfo,
	rep *Report) []Event {
	clockByID := make(map[uint32]*mp4TrackClock)
	for _, ti := range tis {
		clockByID[ti.TrackID] = &mp4TrackClock{ti: ti}
	}
	var events []Event

	// 1) 顶层/moof 内 emsg 盒（v1 绝对呈现时间，v0 为 delta）。
	for _, n := range roots {
		events = append(events, parseEmsgTree(r, n, rep)...)
	}

	// 2) 自定义 id3 盒（AnyType 净荷），按文件出现顺序。
	for _, n := range roots {
		for _, b := range n.All("id3 ") {
			ev, ok := parseID3Box(r, b, rep)
			if ok {
				events = append(events, ev)
			}
		}
	}

	// 3) SEI 与字幕轨：从样本净荷扫描，时钟取所属轨道。
	for _, ti := range tis {
		switch ti.Kind {
		case "video":
			events = append(events, scanSEI(r, ti, rep)...)
		case "subtitle":
			events = append(events, scanSubtitles(r, ti, rep)...)
		}
	}
	return events
}

type mp4TrackClock struct{ ti *TrackInfo }

// parseEmsgTree 遍历整棵树收集 emsg（可能位于 moof 内或顶层）。
func parseEmsgTree(r io.ReadSeeker, root *RawBox, rep *Report) []Event {
	var out []Event
	for _, b := range root.All("emsg") {
		var em mp4.Emsg
		if err := Unmarshal(r, b, &em); err != nil {
			rep.add(&Error{Level: LevelRecoverable, BoxType: "emsg",
				Offset: int64(b.Info.Offset), Message: err.Error()})
			continue
		}
		tick := uint64(em.PresentationTime)
		if em.GetVersion() == 0 {
			// v0 只有 delta，无绝对时间锚点，按 delta 直接记录。
			tick = uint64(em.PresentationTimeDelta)
		}
		out = append(out, Event{
			Kind: EvEmsg, RawTick: tick, Source: b.Type(),
			SchemeURI: em.SchemeIdUri, EventID: em.Id,
			Text: string(em.MessageData),
			StartNs: emsgToNs(em, tick), EndNs: emsgToNs(em, tick+uint64(em.EventDuration)),
		})
	}
	return out
}

func emsgToNs(em mp4.Emsg, tick uint64) int64 {
	if em.Timescale == 0 {
		return 0
	}
	return int64(tick) * 1_000_000_000 / int64(em.Timescale)
}

// parseID3Box 解析自定义 'id3 ' 盒：净荷以 ID3v2 头开始
// （"ID3" + 版本2字节 + flags + 4 字节同步安全长度），随后为帧数据。
func parseID3Box(r io.ReadSeeker, b *RawBox, rep *Report) (Event, bool) {
	payload, err := ReadPayload(r, b, DefaultLimits())
	if err != nil {
		rep.add(&Error{Level: LevelRecoverable, BoxType: "id3 ",
			Offset: int64(b.Info.Offset), Message: err.Error()})
		return Event{}, false
	}
	if len(payload) < 10 || !bytes.Equal(payload[0:3], []byte("ID3")) {
		rep.add(&Error{Level: LevelRecoverable, BoxType: "id3 ",
			Offset: int64(b.Info.Offset), Message: "missing ID3v2 header"})
		return Event{}, false
	}
	// 同步安全整数：每字节低 7 位有效。
	size := syncsafe(payload[6:10])
	end := 10 + int(size)
	if end > len(payload) {
		end = len(payload)
	}
	first := firstID3Frame(payload[10:end])
	return Event{
		Kind: EvID3, Source: b.Type(), RawTick: 0,
		Text: first,
	}, true
}

func syncsafe(b []byte) int {
	return int(b[0])<<21 | int(b[1])<<14 | int(b[2])<<7 | int(b[3])
}

// firstID3Frame 返回首个文本帧（TIT2/TALB/TXXX 等）的可读内容。
func firstID3Frame(body []byte) string {
	pos := 0
	for pos+10 <= len(body) {
		id := string(body[pos : pos+4])
		fsize := int(binary.BigEndian.Uint32(body[pos+4 : pos+8]))
		if fsize <= 0 || pos+10+fsize > len(body) {
			break
		}
		data := body[pos+10 : pos+10+fsize]
		if id[0] == 'T' && len(data) > 1 {
			// 首字节为编码，其余为文本（生成器固定 UTF-8/ISO-8859-1）。
			return fmt.Sprintf("%s=%s", id, bytes.TrimRight(data[1:], "\x00"))
		}
		pos += 10 + fsize
	}
	return ""
}

// scanSEI 在视频样本净荷中扫描 H.264 SEI NAL（type 6），
// 提取 user data unregistered（uuid+payload）作为事件文本。
func scanSEI(r io.ReadSeeker, ti *TrackInfo, rep *Report) []Event {
	var out []Event
	for i := range ti.Samples {
		s := &ti.Samples[i]
		nalType, payload, ok := readFirstNAL(r, s, DefaultLimits())
		if !ok {
			continue
		}
		if nalType != 6 {
			continue
		}
		text := extractSEI(payload)
		ns, errNs, err := ti.Clock.ToNanos(int64(s.RawTick))
		if err != nil {
			rep.add(&Error{Level: LevelRecoverable, BoxType: "timestamp",
				TrackID: ti.TrackID, RawTick: s.RawTick, Message: err.Error()})
			continue
		}
		out = append(out, Event{
			TrackID: ti.TrackID, Kind: EvSEI, RawTick: s.RawTick,
			ErrNs: errNs, StartNs: ns, EndNs: ns + s.DurationNs,
			Source: "nal:6", Text: text,
		})
	}
	return out
}

// readFirstNAL 读取 AVCC 样本首个 NAL，返回类型与净荷。
func readFirstNAL(r io.ReadSeeker, s *Sample, lim Limits) (byte, []byte, bool) {
	n := int64(s.Size)
	if n <= 4 || n > lim.MaxPayload || s.Offset == 0 {
		return 0, nil, false
	}
	if _, err := r.Seek(int64(s.Offset), io.SeekStart); err != nil {
		return 0, nil, false
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(r, head); err != nil {
		return 0, nil, false
	}
	nalLen := int64(binary.BigEndian.Uint32(head))
	if nalLen <= 0 || nalLen+4 > n {
		return 0, nil, false
	}
	buf := make([]byte, nalLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, false
	}
	return buf[0] & 0x1f, buf[1:], true
}

// extractSEI 解析 SEI 净荷：payloadType(变长) + payloadSize(变长) + 数据。
// type 5 = user data unregistered，取 uuid 之后的 ASCII 内容。
func extractSEI(nal []byte) string {
	pos := 0
	for pos < len(nal) {
		ptype := 0
		for pos < len(nal) && nal[pos] == 0xff {
			ptype += 255
			pos++
		}
		if pos >= len(nal) {
			break
		}
		ptype += int(nal[pos])
		pos++
		psize := 0
		for pos < len(nal) && nal[pos] == 0xff {
			psize += 255
			pos++
		}
		if pos >= len(nal) {
			break
		}
		psize += int(nal[pos])
		pos++
		if pos+psize > len(nal) {
			break
		}
		data := nal[pos : pos+psize]
		if ptype == 5 && len(data) > 16 {
			return string(bytes.TrimRight(data[16:], "\x00"))
		}
		pos += psize
	}
	return ""
}

// scanSubtitles 将字幕轨样本转为带起止时间的事件。
// 生成器把每条字幕写成纯文本样本，结束时间取下一样本起点。
func scanSubtitles(r io.ReadSeeker, ti *TrackInfo, rep *Report) []Event {
	var out []Event
	for i := range ti.Samples {
		s := &ti.Samples[i]
		if s.Offset == 0 || s.Size == 0 || int64(s.Size) > DefaultLimits().MaxPayload {
			continue
		}
		buf := make([]byte, s.Size)
		if _, err := r.Seek(int64(s.Offset), io.SeekStart); err != nil {
			rep.add(&Error{Level: LevelRecoverable, BoxType: "subtitle",
				TrackID: ti.TrackID, Message: err.Error()})
			continue
		}
		if _, err := io.ReadFull(r, buf); err != nil {
			rep.add(&Error{Level: LevelRecoverable, BoxType: "subtitle",
				TrackID: ti.TrackID, Message: err.Error()})
			continue
		}
		end := s.TimeNs + s.DurationNs
		if i+1 < len(ti.Samples) {
			end = ti.Samples[i+1].TimeNs
		}
		out = append(out, Event{
			TrackID: ti.TrackID, Kind: EvSubtitle, RawTick: s.RawTick,
			StartNs: s.TimeNs, EndNs: end, Source: "stbl",
			Text: string(bytes.TrimRight(buf, "\x00")),
		})
	}
	return out
}
