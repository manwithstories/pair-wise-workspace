package probe

import (
	"bufio"
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"

	"bcast/internal/fault"
	"bcast/internal/model"
)

// syncBytes 是同步字的字节形式，供同步感知的包体读取使用。
var syncBytes = func() []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, SyncWord)
	return b
}()

// Reader 以流式方式读取 master.bin：内存中只保留当前包与小缓冲，
// 截断后依靠同步字重扫，后续可恢复片段继续交付。
type Reader struct {
	f        *os.File
	br       *bufio.Reader
	plan     model.Plan
	declared uint64
	read     uint64
	offset   int64 // 下一个待读字节的绝对偏移
	lastSeq  map[uint8]uint64
	started  map[uint8]bool
	aligned  bool // 流是否已对齐到下一条记录的同步字
	faults   *fault.Collector
	gaps     []model.Gap
}

// Open 打开素材并解析自描述头部。
func Open(path string, fc *fault.Collector) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(f, 1<<16)
	plan, total, err := ReadHeader(br)
	if err != nil {
		f.Close()
		return nil, fault.New(fault.ErrInvalidInput, "input", 0, 0, 0, 0, err.Error())
	}
	headSize := HeaderSize(len(plan.Tracks))
	return &Reader{
		f: f, br: br, plan: plan, declared: total, offset: headSize,
		lastSeq: map[uint8]uint64{}, started: map[uint8]bool{},
		faults: fc,
	}, nil
}

// Plan 返回素材的生成参数清单。
func (r *Reader) Plan() model.Plan { return r.plan }

// Close 释放底层文件。
func (r *Reader) Close() error { return r.f.Close() }

// Gaps 返回读取过程中识别的全部丢包缺口。
func (r *Reader) Gaps() []model.Gap {
	out := make([]model.Gap, len(r.gaps))
	copy(out, r.gaps)
	return out
}

// ReadPacket 交付下一个可恢复包。布尔为 false 表示素材正常结束。
// 截断/校验类故障写入 Collector，不终止整体读取。
func (r *Reader) ReadPacket() (model.Packet, bool, error) {
	for {
		pkt, ok, err := r.readOne()
		if err != nil || !ok {
			return model.Packet{}, false, err
		}
		r.detectGap(pkt)
		r.read++
		return pkt, true, nil
	}
}

func (r *Reader) readOne() (model.Packet, bool, error) {
	start := r.offset
	hdr, err := r.readRecordHeader()
	if err == io.EOF {
		return model.Packet{}, false, nil // 干净的记录边界上结束
	}
	if err != nil {
		// 截断：头部不完整，记录故障并尝试重扫后续同步字。
		r.faults.Record(fault.KindTruncated, 0, 0, start, r.offset, err.Error())
		if !r.resync() {
			return model.Packet{}, false, nil
		}
		return r.readOne()
	}
	plen := int(binary.BigEndian.Uint32(hdr[26:30]))
	if plen <= 0 || plen > MaxPacketLen {
		r.faults.Record(fault.KindCorrupt, hdr[4], binary.BigEndian.Uint64(hdr[6:14]),
			start, r.offset+int64(plen), "declared length out of bounds")
		if !r.resync() {
			return model.Packet{}, false, nil
		}
		return r.readOne()
	}
	body, perr := r.readPayload(plen)
	pkt := model.Packet{
		TrackID:   hdr[4],
		Kind:      model.TrackKind(hdr[5]),
		Seq:       binary.BigEndian.Uint64(hdr[6:14]),
		PTS:       binary.BigEndian.Uint64(hdr[14:22]),
		Duration:  binary.BigEndian.Uint32(hdr[22:26]),
		ByteStart: start,
		ByteEnd:   r.offset,
		Checksum:  binary.BigEndian.Uint32(hdr[30:34]),
	}
	if perr != nil {
		// 截断（中段短包或尾部 EOF）：保留已恢复部分，流已对齐到下一记录。
		pkt.Payload = body
		r.faults.Record(fault.KindTruncated, pkt.TrackID, pkt.Seq, start, r.offset, perr.Error())
		return pkt, true, nil
	}
	pkt.Payload = body
	if crc32.ChecksumIEEE(body) != pkt.Checksum {
		r.faults.Record(fault.KindChecksum, pkt.TrackID, pkt.Seq, start, r.offset,
			"crc-32 verification failed")
	}
	return pkt, true, nil
}

// readPayload 逐字节读取声明长度的包体，并用滚动窗口识别中途提前出现的
// 同步字（中段截断）。正常返回 nil 错误；截断时返回已恢复字节与错误，且
// 流已停在下一记录的对齐位置（aligned=true，同步字已被消费），尾部 EOF 同理。
func (r *Reader) readPayload(plen int) ([]byte, error) {
	out := make([]byte, 0, plen)
	match := 0 // 已匹配的同步字前缀长度
	for len(out)+match < plen {
		b, err := r.br.ReadByte()
		if err != nil {
			// 尾部 EOF：先结算已匹配的前缀字节，再返回恢复部分。
			out = append(out, syncBytes[:match]...)
			return out, io.ErrUnexpectedEOF
		}
		r.offset++
		if b == syncBytes[match] {
			match++
			if match == 4 {
				// 完整同步字：当前包被截断，同步字属下一记录，已消费且对齐。
				r.aligned = true
				return out, io.ErrUnexpectedEOF
			}
			continue
		}
		// 匹配断裂：已缓存的前缀是普通包体字节，需连同当前字节一起结算。
		out = append(out, syncBytes[:match]...)
		out = append(out, b)
		match = 0
	}
	// 凑满声明长度；若末尾恰好停在同步字前缀上，这些字节仍属本包。
	out = append(out, syncBytes[:match]...)
	return out, nil
}

// readRecordHeader 读取同步字与固定头部；流在干净 EOF 结束时返回 io.EOF。
func (r *Reader) readRecordHeader() ([]byte, error) {
	if err := r.ensureSync(); err != nil {
		return nil, err
	}
	// ensureSync 已消费 4 字节同步字，这里只读其后的固定字段。
	rest := make([]byte, RecHdrLen-4)
	if _, err := io.ReadFull(r.br, rest); err != nil {
		r.aligned = false
		return nil, err
	}
	r.offset += RecHdrLen - 4
	r.aligned = false // 头部已消费，下一条记录需重新对齐
	hdr := make([]byte, RecHdrLen)
	u32(hdr[0:4], SyncWord)
	copy(hdr[4:], rest)
	return hdr, nil
}

// ensureSync 逐字节滑动匹配 4 字节同步字，命中时同步字已被完整消费，
// 后续 ReadFull 直接读取头部剩余字段。干净记录边界上的 EOF 返回 io.EOF；
// 读到杂散字节后才遇 EOF 视作尾部截断。
func (r *Reader) ensureSync() error {
	if r.aligned {
		return nil
	}
	var word [4]byte
	i := 0
	junk := false
	for {
		b, err := r.br.ReadByte()
		if err == io.EOF {
			if i == 0 && !junk {
				return io.EOF
			}
			return io.ErrUnexpectedEOF
		}
		if err != nil {
			return err
		}
		r.offset++
		word[i] = b
		i++
		if i < 4 {
			continue
		}
		if binary.BigEndian.Uint32(word[:]) == SyncWord {
			r.aligned = true
			return nil
		}
		// 滑动窗口：丢弃首字节，保留后三字节继续匹配，被丢字节计入杂散。
		copy(word[:], word[1:])
		i = 3
		junk = true
	}
}

// resync 在截断或损坏后对齐下一条同步字，返回是否找到可恢复记录。
func (r *Reader) resync() bool {
	err := r.ensureSync()
	return err == nil && r.aligned
}

// detectGap 依据轨道内序号断号记录丢包缺口（重复包不断号）。
func (r *Reader) detectGap(pkt model.Packet) {
	if !r.started[pkt.TrackID] {
		r.started[pkt.TrackID] = true
		r.lastSeq[pkt.TrackID] = pkt.Seq
		return
	}
	last := r.lastSeq[pkt.TrackID]
	switch {
	case pkt.Seq == last:
		return // 重复包
	case pkt.Seq > last+1:
		missing := make([]uint64, 0, pkt.Seq-last-1)
		for s := last + 1; s < pkt.Seq; s++ {
			missing = append(missing, s)
		}
		r.gaps = append(r.gaps, model.Gap{
			TrackID: pkt.TrackID, Kind: pkt.Kind, Missing: missing,
			AfterSeq: last, BeforeSeq: pkt.Seq,
		})
		r.faults.Record(fault.KindDrop, pkt.TrackID, last+1, 0, 0, "sequence gap detected")
	case pkt.Seq < last:
		r.faults.Record(fault.KindWrap, pkt.TrackID, pkt.Seq, pkt.ByteStart, pkt.ByteEnd,
			"sequence number moved backwards")
	}
	r.lastSeq[pkt.TrackID] = pkt.Seq
}

// ReadCount 返回已成功交付的包数（含重复与损坏但可恢复的包）。
func (r *Reader) ReadCount() uint64 { return r.read }

// DeclaredCount 返回头部声明的写入包总数。
func (r *Reader) DeclaredCount() uint64 { return r.declared }
