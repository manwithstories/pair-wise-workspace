// arc1_parse.go 实现 Parser 接口：探针、限制校验、回绕展开与损坏尾包处理。
package adapter

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"io"
	"os"

	"archivemedia/internal/model"
)

// Arc1Parser 是纯 Go 归档容器适配器。
type Arc1Parser struct{}

// 编译期断言：Arc1Parser 满足 Parser。
var _ Parser = (*Arc1Parser)(nil)

func init() { MustRegister(&Arc1Parser{}) }

func (p *Arc1Parser) Name() string    { return ARCAdapterName }
func (p *Arc1Parser) Version() string { return ARCAdapterVer }
func (p *Arc1Parser) Capabilities() []Capability {
	return []Capability{CapDemux, CapDecode, CapWrapExpand, CapTruncation}
}

// Probe 只读取前 4 字节魔数。
func (p *Arc1Parser) Probe(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, model.Wrap(model.KindInvalidArgument, err, "arc1: open %s", path)
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		// 过短的文件不归属本适配器，交由协商流程报 unsupported。
		return false, nil
	}
	return string(magic[:]) == ARCMagic, nil
}

// Parse 解析整个容器并展开时间戳回绕。
func (p *Arc1Parser) Parse(path string, opts ParseOptions) (*ParsedMedia, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, model.Wrap(model.KindInvalidArgument, err, "arc1: stat %s", path)
	}
	if opts.MaxFileBytes > 0 && info.Size() > opts.MaxFileBytes {
		return nil, model.NewError(model.KindQuotaExceeded,
			"arc1: file %d bytes exceeds limit %d", info.Size(), opts.MaxFileBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, model.Wrap(model.KindInvalidArgument, err, "arc1: open %s", path)
	}
	defer f.Close()

	r := &countingReader{r: f}
	pm := &ParsedMedia{SourceID: path, AdapterName: ARCAdapterName, AdapterVersion: ARCAdapterVer,
		Container: "arc1", FileSize: info.Size()}
	if err := p.readHeader(r, pm, opts); err != nil {
		return nil, err
	}
	wrapState := make([]wrapTracker, len(pm.Tracks))
	for i, t := range pm.Tracks {
		wrapState[i].modulo = int64(1) << uint(t.WrapBits)
	}
	var corrupt *model.MediaError
	for {
		pkt, perr := p.readPacket(r, pm, wrapState, opts)
		if perr != nil {
			if model.KindOf(perr) == model.KindCorruptMedia {
				corrupt = perr.(*model.MediaError)
				break
			}
			return nil, perr
		}
		if pkt == nil {
			break
		}
		pm.Packets = append(pm.Packets, *pkt)
	}
	pm.VerifiedPrefix = r.verified
	if corrupt != nil {
		pm.Truncated = true
		pm.FirstBadAt = corrupt.FirstBadAt
		// 保留已验证前缀：返回可用结果 + 结构化错误，由上层决定隔离或续跑。
		return pm, corrupt
	}
	return pm, nil
}

func (p *Arc1Parser) readHeader(r *countingReader, pm *ParsedMedia, opts ParseOptions) error {
	var head [8]byte
	if err := r.readFull( head[:]); err != nil {
		return model.NewError(model.KindUnsupportedFormat, "arc1: unreadable header: %v", err)
	}
	if string(head[0:4]) != ARCMagic {
		return model.NewError(model.KindUnsupportedFormat, "arc1: bad magic")
	}
	if head[4] != ARCVersion {
		return model.NewError(model.KindUnsupportedFormat,
			"arc1: unsupported container version %d (want %d)", head[4], ARCVersion)
	}
	// flags(head[5]) 的 truncated 位仅作提示，真正判定以 EOF 为准。
	n := int(binary.LittleEndian.Uint16(head[6:8]))
	if opts.MaxTracks > 0 && n > opts.MaxTracks {
		return model.NewError(model.KindQuotaExceeded, "arc1: %d tracks exceed limit %d", n, opts.MaxTracks)
	}
	for i := 0; i < n; i++ {
		t, err := p.readTrack(r)
		if err != nil {
			return err
		}
		t.Index = i
		pm.Tracks = append(pm.Tracks, t)
	}
	return nil
}

func (p *Arc1Parser) readTrack(r *countingReader) (Track, error) {
	var fixed [2]byte
	if err := r.readFull( fixed[:]); err != nil {
		return Track{}, model.Wrap(model.KindCorruptMedia, err, "arc1: track header truncated")
	}
	mt := MediaType(fixed[0])
	codecLen := int(fixed[1])
	codec := make([]byte, codecLen)
	if err := r.readFull( codec); err != nil {
		return Track{}, model.Wrap(model.KindCorruptMedia, err, "arc1: codec field truncated")
	}
	body := make([]byte, 8+8+4+4+4+2+1)
	if err := r.readFull( body); err != nil {
		return Track{}, model.Wrap(model.KindCorruptMedia, err, "arc1: track body truncated")
	}
	o := 0
	num := int64(binary.LittleEndian.Uint64(body[o:]))
	o += 8
	den := int64(binary.LittleEndian.Uint64(body[o:]))
	o += 8
	tb, err := model.NewRational(num, den)
	if err != nil {
		return Track{}, model.NewError(model.KindUnsupportedFormat, "arc1: invalid timebase %d/%d", num, den)
	}
	t := Track{
		Type:       mt,
		Codec:      string(codec),
		TimeBase:   tb,
		Width:      int(binary.LittleEndian.Uint32(body[o:])),
		Height:     int(binary.LittleEndian.Uint32(body[o+4:])),
		SampleRate: int(binary.LittleEndian.Uint32(body[o+8:])),
		Channels:   int(binary.LittleEndian.Uint16(body[o+12:])),
		WrapBits:   codeToWrapBits(body[o+14]),
	}
	return t, nil
}

// wrapTracker 维护单轨的回绕展开状态：expanded = raw + cycles*modulo。
type wrapTracker struct {
	modulo int64
	cycles int64
	prev   int64
	primed bool
	wraps  []WrapEvent
}

func (p *Arc1Parser) readPacket(r *countingReader, pm *ParsedMedia, st []wrapTracker,
	opts ParseOptions) (*FramePacket, error) {
	hdr := make([]byte, 2+8+8+4+1+8)
	if err := r.readFull( hdr); err != nil {
		if err == io.EOF {
			return nil, nil // 干净的文件结束
		}
		return nil, model.NewError(model.KindCorruptMedia, "arc1: packet header truncated").
			WithFirstBad(pm.FirstBadAt)
	}
	o := 0
	trackIdx := int(binary.LittleEndian.Uint16(hdr[o:]))
	o += 2
	start := int64(binary.LittleEndian.Uint64(hdr[o:]))
	o += 8
	dur := int64(binary.LittleEndian.Uint64(hdr[o:]))
	o += 8
	payloadLen := int(binary.LittleEndian.Uint32(hdr[o:]))
	o += 4
	flags := hdr[o]
	o += 1
	wantHash := binary.LittleEndian.Uint64(hdr[o:])
	if trackIdx >= len(pm.Tracks) {
		return nil, model.NewError(model.KindCorruptMedia, "arc1: packet references unknown track %d", trackIdx)
	}
	payload := make([]byte, payloadLen)
	if err := r.readFull( payload); err != nil {
		// 尾包损坏：记录首个坏时间（按该轨时基尽力换算）。
		bad := model.TicksToTime(st[trackIdx].cycles*st[trackIdx].modulo+start, pm.Tracks[trackIdx].TimeBase)
		return nil, model.NewError(model.KindCorruptMedia,
			"arc1: payload truncated at track %d: %v", trackIdx, err).
			WithSource(pm.SourceID).WithFirstBad(bad)
	}
	// 内容哈希校验：损坏负载在此被拦截，不污染后续片段。
	got := fnv.New64a()
	got.Write(payload)
	if opts.ComputeHash && got.Sum64() != wantHash {
		bad := model.TicksToTime(start, pm.Tracks[trackIdx].TimeBase)
		return nil, model.NewError(model.KindCorruptMedia, "arc1: content hash mismatch").
			WithSource(pm.SourceID).WithFirstBad(bad)
	}
	// 回绕展开：检测到 raw 倒退且该轨声明了位宽，则增加周期。
	ws := &st[trackIdx]
	expanded := start
	if ws.primed && ws.modulo > 1 && start < ws.prev {
		ws.cycles++
		pm.Wraps = append(pm.Wraps, WrapEvent{
			TrackIndex: trackIdx, AtTicks: ws.prev, High: ws.prev, Low: start,
			WrapBits: pm.Tracks[trackIdx].WrapBits,
		})
		expanded = start + ws.cycles*ws.modulo
	} else if ws.primed && ws.modulo > 1 {
		expanded = start + ws.cycles*ws.modulo
	}
	ws.prev = start
	ws.primed = true
	// 时长上限（按归一化微秒粗算）。
	if opts.MaxDurationUs > 0 {
		now := model.TicksToTime(expanded, pm.Tracks[trackIdx].TimeBase).Microseconds()
		if now > opts.MaxDurationUs {
			return nil, model.NewError(model.KindQuotaExceeded,
				"arc1: decode duration %dus exceeds limit %d", now, opts.MaxDurationUs)
		}
	}
	return &FramePacket{
		TrackIndex:    trackIdx,
		StartTicks:    expanded,
		DurationTicks: dur,
		KeyFrame:      flags&flagKeyFrame != 0,
		ContentHash:   wantHash,
		PayloadSize:   payloadLen,
		RawPayload:    payload,
	}, nil
}

// countingReader 同时统计物理读取字节与已验证字节：
// 只有完整成功的读取才推进 verified，残缺尾包不计入已验证前缀。
type countingReader struct {
	r        io.Reader
	read     int64
	verified int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	if err == nil {
		c.verified += int64(n)
	}
	return n, err
}

// readFull 仅在全部字节读取成功时推进 verified 计数。
func (c *countingReader) readFull(buf []byte) error {
	start := c.verified
	if _, err := io.ReadFull(c, buf); err != nil {
		// 回退本次读取中被计入的完整部分。
		c.verified = start
		return err
	}
	return nil
}

// String 供终端诊断输出。
func (pm *ParsedMedia) String() string {
	return fmt.Sprintf("media(%s via %s@%s, tracks=%d packets=%d wraps=%d truncated=%v)",
		pm.SourceID, pm.AdapterName, pm.AdapterVersion, len(pm.Tracks), len(pm.Packets),
		len(pm.Wraps), pm.Truncated)
}
