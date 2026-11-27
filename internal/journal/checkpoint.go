// Package journal 实现断点续跑所需的原子检查点与校验清单。
//
// 检查点采用"临时文件 + fsync + 原子 rename"写入；读取时校验魔数、版本与
// SHA-256，截断或校验失败一律回退到前一有效检查点，保证续跑不会重复计入包。
package journal

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"archivemedia/internal/media"
)

const (
	cpMagic   uint32 = 0x4350_4B31 // "CPK1"
	cpVersion uint16 = 1
	// cpHeader: magic(4)+version(2)+payloadLen(4)+seq(8)+sha(32) = 50
	cpHeaderLen = 50
	shaLen      = sha256.Size
)

// Checkpoint 是一次流水线进度快照。
type Checkpoint struct {
	// Seq 单调递增，每次成功写入 +1，用于区分新旧检查点。
	Seq uint64
	// LastExtSeq 是已确认处理的最大展开序号。
	LastExtSeq int64
	// WatermarkMicros 是检查点时刻水位（微秒）。
	WatermarkMicros int64
	// CountedPackets 是已计入结果的唯一包数；续跑据此跳过重复包。
	CountedPackets uint64
	// OutputOffset 是输出 WAV 已落盘的字节偏移。
	OutputOffset int64
	// InputDigest 是输入事件序列的确定性摘要（十六进制）。
	InputDigest string
	// Payload 是流水线自定义状态（如时间线/音频位置），原样透传。
	Payload []byte
}

// Store 管理一对轮换检查点文件 current/previous，实现坏点回退。
type Store struct {
	dir      string
	curPath  string
	prevPath string
}

// NewStore 在 dir 下创建检查点存储（目录不存在则创建）。
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, media.E(media.KindArgument, "journal.new-store",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"检查点目录为空", nil)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, media.E(media.KindCheckpoint, "journal.new-store",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"创建检查点目录失败", err)
	}
	return &Store{
		dir:      dir,
		curPath:  filepath.Join(dir, "checkpoint.current"),
		prevPath: filepath.Join(dir, "checkpoint.previous"),
	}, nil
}

// encode 序列化检查点：定长头 + 载荷，头部内嵌载荷 SHA-256。
func (c *Checkpoint) encode() ([]byte, error) {
	payload := c.marshalPayload()
	sum := sha256.Sum256(payload)
	buf := make([]byte, cpHeaderLen+len(payload))
	binary.LittleEndian.PutUint32(buf[0:], cpMagic)
	binary.LittleEndian.PutUint16(buf[4:], cpVersion)
	binary.LittleEndian.PutUint32(buf[6:], uint32(len(payload)))
	binary.LittleEndian.PutUint64(buf[10:], c.Seq)
	copy(buf[18:18+shaLen], sum[:])
	copy(buf[cpHeaderLen:], payload)
	return buf, nil
}

// marshalPayload 以固定小端布局编码业务字段 + 自定义载荷。
func (c *Checkpoint) marshalPayload() []byte {
	// LastExtSeq(8)+Watermark(8)+Counted(8)+OutOffset(8)+DigestLen(2)+Digest+Payload
	digest := []byte(c.InputDigest)
	buf := make([]byte, 34+len(digest)+len(c.Payload))
	binary.LittleEndian.PutUint64(buf[0:], uint64(c.LastExtSeq))
	binary.LittleEndian.PutUint64(buf[8:], uint64(c.WatermarkMicros))
	binary.LittleEndian.PutUint64(buf[16:], c.CountedPackets)
	binary.LittleEndian.PutUint64(buf[24:], uint64(c.OutputOffset))
	binary.LittleEndian.PutUint16(buf[32:], uint16(len(digest)))
	copy(buf[34:], digest)
	copy(buf[34+len(digest):], c.Payload)
	return buf
}

// Save 原子写入检查点：先把当前 current 轮换为 previous，再写新 current。
// 任一步崩溃，恢复时都至少有一个有效检查点可用。
func (s *Store) Save(c *Checkpoint) error {
	if c == nil {
		return media.E(media.KindArgument, "journal.save",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"检查点为 nil", nil)
	}
	data, err := c.encode()
	if err != nil {
		return err
	}
	// 轮换：current -> previous（忽略不存在的情况）。
	if _, statErr := os.Stat(s.curPath); statErr == nil {
		if err := os.Rename(s.curPath, s.prevPath); err != nil {
			return media.E(media.KindCheckpoint, "journal.rotate",
				media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
				"轮换检查点失败", err)
		}
	}
	if err := atomicWrite(s.curPath, data); err != nil {
		return media.Wrap(err, "journal.save", "写入当前检查点失败")
	}
	return nil
}

// Load 读取最新有效检查点。current 损坏（截断/校验失败）时自动回退 previous。
// 两个文件都无效时返回 KindCheckpoint 错误，调用方按全新任务处理。
func (s *Store) Load() (*Checkpoint, error) {
	cp, err := readCheckpoint(s.curPath)
	if err == nil {
		return cp, nil
	}
	// current 不可用，尝试 previous。
	prev, perr := readCheckpoint(s.prevPath)
	if perr == nil {
		return prev, nil
	}
	if errors.Is(err, os.ErrNotExist) && errors.Is(perr, os.ErrNotExist) {
		return nil, media.E(media.KindCheckpoint, "journal.load",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"无任何检查点，按全新任务启动", nil)
	}
	return nil, media.E(media.KindCheckpoint, "journal.load",
		media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
		"当前与前一检查点均无效，回退失败", err)
}

// readCheckpoint 解析并完整校验单个检查点文件。
func readCheckpoint(path string) (*Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < cpHeaderLen {
		return nil, media.E(media.KindCheckpoint, "journal.read",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"检查点被截断", nil)
	}
	if magic := binary.LittleEndian.Uint32(data[0:]); magic != cpMagic {
		return nil, media.E(media.KindCheckpoint, "journal.read",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"检查点魔数错误", nil)
	}
	if ver := binary.LittleEndian.Uint16(data[4:]); ver != cpVersion {
		return nil, media.E(media.KindCheckpoint, "journal.read",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"检查点版本不受支持", nil)
	}
	payloadLen := binary.LittleEndian.Uint32(data[6:])
	seq := binary.LittleEndian.Uint64(data[10:])
	var wantSHA [shaLen]byte
	copy(wantSHA[:], data[18:18+shaLen])
	if int(payloadLen) != len(data)-cpHeaderLen {
		return nil, media.E(media.KindCheckpoint, "journal.read",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"检查点载荷长度不匹配（疑似截断）", nil)
	}
	payload := data[cpHeaderLen:]
	gotSHA := sha256.Sum256(payload)
	if gotSHA != wantSHA {
		return nil, media.E(media.KindCheckpoint, "journal.read",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"检查点 SHA-256 校验失败", nil)
	}
	cp, err := unmarshalPayload(seq, payload)
	if err != nil {
		return nil, err
	}
	return cp, nil
}

func unmarshalPayload(seq uint64, b []byte) (*Checkpoint, error) {
	if len(b) < 34 {
		return nil, media.E(media.KindCheckpoint, "journal.read",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"检查点业务载荷过短", nil)
	}
	digestLen := int(binary.LittleEndian.Uint16(b[32:]))
	if 34+digestLen > len(b) {
		return nil, media.E(media.KindCheckpoint, "journal.read",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"摘要长度越界", nil)
	}
	cp := &Checkpoint{
		Seq:             seq,
		LastExtSeq:      int64(binary.LittleEndian.Uint64(b[0:])),
		WatermarkMicros: int64(binary.LittleEndian.Uint64(b[8:])),
		CountedPackets:  binary.LittleEndian.Uint64(b[16:]),
		OutputOffset:    int64(binary.LittleEndian.Uint64(b[24:])),
		InputDigest:     string(b[34 : 34+digestLen]),
		Payload:         append([]byte(nil), b[34+digestLen:]...),
	}
	return cp, nil
}

// atomicWrite 写临时文件、fsync 后原子 rename 到目标路径。
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cp-*")
	if err != nil {
		return media.E(media.KindCheckpoint, "journal.atomic-write",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"创建临时检查点失败", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return media.E(media.KindCheckpoint, "journal.atomic-write",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"写检查点数据失败", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return media.E(media.KindCheckpoint, "journal.atomic-write",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"fsync 检查点失败", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return media.E(media.KindCheckpoint, "journal.atomic-write",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"关闭检查点失败", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return media.E(media.KindCheckpoint, "journal.atomic-write",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"原子替换检查点失败", err)
	}
	return nil
}

// DigestHex 返回数据的 SHA-256 十六进制摘要，供清单与检查点共用。
func DigestHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
