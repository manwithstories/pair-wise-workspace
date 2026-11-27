package resume

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"bcast/internal/fault"
	"bcast/internal/model"
)

// 确定性容器 .mpk 的格式常量。全部整数用大端定宽编码，无长度前缀的
// 变长结构，保证同输入在完整运行与续跑两条路径下逐字节一致。
const (
	MPKMagic    = "MPK00001"
	MPKHeader   = 8 + 8 + 8 + 4 + 4 // magic + segmentUS + sampleN + gapN + faultN
	MPKSampleSz = 1 + 1 + 8 + 8 + 8 // track + kind + seq + pts + duration
	MPKGapSz    = 1 + 8 + 8 + 8 + 8 // track + afterSeq + beforeSeq + start + end
	BlockMarker = uint32(0xB10C5E6)
	BlockHdr    = 4 + 4 // marker + segment index
)

// Manifest 是时间线清单的确定序列化视图（不含 map）。
type Manifest struct {
	SegmentUS    int64
	SampleCount  int
	GapCount     int
	FaultCount   int
	SourceSHA256 string
}

// Writer 把时间线样本按主时钟网格分段写入确定性容器，并驱动检查点。
type Writer struct {
	path      string
	journal   *Journal
	jPath     string
	segmentUS int64
	source    string
	segIndex  int
	off       int64
	f         *os.File
	cutAtUS   int64 // >0 时模拟中断的截止时间（微秒）
}

// NewFullWriter 打开一次完整（或续跑）写入。已有检查点则进入续跑模式，
// 已提交段不再重放，输出文件按检查点偏移对齐。
func NewFullWriter(path, journalPath string, sourceSHA string, segmentUS int64,
	j *Journal) (*Writer, error) {
	if segmentUS <= 0 {
		return nil, fault.InvalidInputf("segment size must be positive, got %d", segmentUS)
	}
	if j == nil {
		j = &Journal{SchemaVersion: SchemaVersion, SourceSHA256: sourceSHA, SegmentUS: segmentUS}
	} else if err := j.BindSource(sourceSHA); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(journalPath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	w := &Writer{path: path, journal: j, jPath: journalPath, segmentUS: segmentUS,
		source: sourceSHA, f: f}
	if len(j.Committed) > 0 {
		// 续跑：定位到已提交末尾，下一段从最后提交段 +1 开始。
		last := j.Committed[len(j.Committed)-1]
		w.off = last.EndOffset
		w.segIndex = last.Segment + 1
		// 截断掉上次失败运行残留的未提交尾部，避免污染块校验。
		if err := f.Truncate(w.off); err != nil {
			f.Close()
			return nil, err
		}
		if _, err := f.Seek(w.off, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
	}
	return w, nil
}

// SetCutPoint 注入模拟中断：写入的样本时间一旦 >= cutAtUS 即返回 ErrCut。
func (w *Writer) SetCutPoint(cutAtUS int64) { w.cutAtUS = cutAtUS }

// ErrCut 表示到达模拟中断点，调用方应正常关闭并据此演示续跑。
var ErrCut = errors.New("resume: simulated interruption cut point reached")

// WriteAll 按段分组样本与缺口并逐段提交；遇到中断点返回 ErrCut。
func (w *Writer) WriteAll(samples []model.Sample, gaps []model.Gap,
	faults []model.Fault, header Manifest) error {
	if err := w.ensureHeader(header); err != nil {
		return err
	}
	maxSeg := 0
	for _, s := range samples {
		if idx := int(s.PTS / w.segmentUS); idx > maxSeg {
			maxSeg = idx
		}
	}
	for ; w.segIndex <= maxSeg; w.segIndex++ {
		lo := int64(w.segIndex) * w.segmentUS
		hi := lo + w.segmentUS
		segSamples := filterSamples(samples, lo, hi)
		segGaps := filterGaps(gaps, lo, hi)
		if w.cutAtUS > 0 && lo >= w.cutAtUS {
			return ErrCut
		}
		if err := w.writeBlock(w.segIndex, segSamples, segGaps); err != nil {
			return err
		}
		if w.cutAtUS > 0 && hi > w.cutAtUS {
			return ErrCut
		}
	}
	return nil
}

// ensureHeader 仅在首次运行写入固定头部；续跑时头部已存在，跳过。
func (w *Writer) ensureHeader(m Manifest) error {
	if len(w.journal.Committed) > 0 {
		return nil
	}
	buf := make([]byte, MPKHeader)
	copy(buf[0:8], MPKMagic)
	binary.BigEndian.PutUint64(buf[8:16], uint64(w.segmentUS))
	binary.BigEndian.PutUint64(buf[16:24], uint64(m.SampleCount))
	binary.BigEndian.PutUint32(buf[24:28], uint32(m.GapCount))
	binary.BigEndian.PutUint32(buf[28:32], uint32(m.FaultCount))
	n, err := w.f.WriteAt(buf, w.off)
	if err != nil {
		return err
	}
	w.off += int64(n)
	return nil
}

// writeBlock 序列化一个段块，计算块体哈希并原子提交检查点。
func (w *Writer) writeBlock(segIndex int, samples []model.Sample, gaps []model.Gap) error {
	body := encodeBlock(samples, gaps)
	hdr := make([]byte, BlockHdr)
	binary.BigEndian.PutUint32(hdr[0:4], BlockMarker)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(segIndex))
	if _, err := w.f.WriteAt(hdr, w.off); err != nil {
		return err
	}
	if _, err := w.f.WriteAt(body, w.off+BlockHdr); err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	endOffset := w.off + BlockHdr + int64(len(body))
	cp := Checkpoint{
		Segment: segIndex, SampleCount: len(samples),
		EndOffset: endOffset, BlockSHA256: hex.EncodeToString(sum[:]),
	}
	if err := w.journal.Commit(w.jPath, cp); err != nil {
		return err
	}
	w.off = endOffset
	return nil
}

// Close 刷盘并关闭底层文件。
func (w *Writer) Close() error {
	if err := w.f.Sync(); err != nil {
		return err
	}
	return w.f.Close()
}

// encodeBlock 产出段块的确定性字节：定宽样本记录后接定宽缺口记录。
func encodeBlock(samples []model.Sample, gaps []model.Gap) []byte {
	buf := make([]byte, 0, len(samples)*MPKSampleSz+len(gaps)*MPKGapSz+8)
	tmp := make([]byte, 8)
	for _, s := range samples {
		row := make([]byte, MPKSampleSz)
		row[0] = s.TrackID
		row[1] = uint8(s.Kind)
		binary.BigEndian.PutUint64(row[2:10], s.Seq)
		binary.BigEndian.PutUint64(row[10:18], uint64(s.PTS))
		binary.BigEndian.PutUint64(row[18:26], uint64(s.Duration))
		buf = append(buf, row...)
	}
	for _, g := range gaps {
		row := make([]byte, MPKGapSz)
		row[0] = g.TrackID
		binary.BigEndian.PutUint64(row[1:9], g.AfterSeq)
		binary.BigEndian.PutUint64(row[9:17], g.BeforeSeq)
		binary.BigEndian.PutUint64(row[17:25], uint64(g.StartPTS))
		binary.BigEndian.PutUint64(row[25:33], uint64(g.EndPTS))
		buf = append(buf, row...)
	}
	// 块尾写样本/缺口计数，读取侧据此切分定宽记录。
	binary.BigEndian.PutUint32(tmp[0:4], uint32(len(samples)))
	binary.BigEndian.PutUint32(tmp[4:8], uint32(len(gaps)))
	buf = append(buf, tmp...)
	return buf
}

func filterSamples(in []model.Sample, lo, hi int64) []model.Sample {
	out := make([]model.Sample, 0)
	for _, s := range in {
		if s.PTS >= lo && s.PTS < hi {
			out = append(out, s)
		}
	}
	return out
}

func filterGaps(in []model.Gap, lo, hi int64) []model.Gap {
	out := make([]model.Gap, 0)
	for _, g := range in {
		if g.StartPTS >= lo && g.StartPTS < hi {
			out = append(out, g)
		}
	}
	return out
}

// FileSHA256 返回指定文件的十六进制 SHA256，供最终校验和比对。
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyBlocks 独立重放容器全部块，逐块比对检查点哈希与文件偏移。
// 任何不一致都包装为 fault.ErrChecksum / fault.ErrBoundary 上抛。
func VerifyBlocks(path string, j *Journal) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	off := int64(MPKHeader)
	for idx, cp := range j.Committed {
		if int(off+BlockHdr) > len(data) {
			return fault.Boundaryf("block %d header beyond file", idx)
		}
		if binary.BigEndian.Uint32(data[off:off+4]) != BlockMarker {
			return fault.Boundaryf("block %d missing marker", idx)
		}
		if got := binary.BigEndian.Uint32(data[off+4 : off+8]); int(got) != cp.Segment {
			return fault.Boundaryf("block %d segment id %d", idx, got)
		}
		bodyEnd := cp.EndOffset
		if bodyEnd > int64(len(data)) {
			return fault.Boundaryf("block %d end beyond file", idx)
		}
		body := data[off+BlockHdr : bodyEnd]
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != cp.BlockSHA256 {
			return fault.New(fault.ErrChecksum, fault.KindResume, 0, uint64(cp.Segment),
				off, bodyEnd, "block hash mismatch")
		}
		off = cp.EndOffset
	}
	if off != int64(len(data)) {
		return fault.Boundaryf("trailing %d bytes after last committed block", int64(len(data))-off)
	}
	return nil
}

// String 给出容器的可读摘要，供终端输出与清单使用。
func (m Manifest) String() string {
	return fmt.Sprintf("samples=%d gaps=%d faults=%d segmentUS=%d source=%s",
		m.SampleCount, m.GapCount, m.FaultCount, m.SegmentUS,
		m.SourceSHA256[:min(12, len(m.SourceSHA256))])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
