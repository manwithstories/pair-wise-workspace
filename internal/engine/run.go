package engine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"relay/internal/audio"
	"relay/internal/index"
	"relay/internal/media"
	"relay/internal/rtp"
)

// Config 是一次处理运行的全部输入。
type Config struct {
	InputPath           string
	OutputPath          string
	WorkDir             string
	Params              index.Params
	InjectWriteFailAfter int           // >0 时，第 N 次检查点提交后注入一次写盘失败
	InterruptAt         time.Duration // >0 时，运行该时长后模拟进程中断
}

// Report 汇总一次运行的终端可见结果。
type Report struct {
	DamagePacket  int64 // 注入的坏包序号（-1 表示无）
	LocatedPacket int64 // 实际定位到的坏包序号（-1 表示无）
	LocateDelta   int64 // 定位误差（包数）
	ResumeMS      int64 // 恢复耗时（毫秒）
	OutRate       int
	Channels      int
	LUFS          float64
	ChecksumA     string
	ChecksumB     string
	ChecksumMatch bool
	PPM           int
	Missing       int64
}

const checkpointEvery = 128

var (
	errInterrupted = errors.New("simulated interrupt")
	errDamaged     = errors.New("damaged input")
)

// Run 执行扫描—测量—渲染流水线，检查点驱动中断/写盘失败后的续跑。
func Run(cfg Config) (*Report, error) {
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return nil, &media.IOError{Op: "mkdir", Path: cfg.WorkDir, Reason: err.Error()}
	}
	idxStore := index.NewStore(cfg.WorkDir + "/index.json")
	cpStore := NewCheckpointStore(cfg.WorkDir + "/checkpoint.json")

	inputDigest, err := index.InputDigest(cfg.InputPath)
	if err != nil {
		return nil, err
	}
	idx, err := loadOrScan(idxStore, cfg.InputPath, inputDigest, cfg.Params)
	if err != nil {
		return nil, mapScanError(err)
	}

	rep := &Report{
		OutRate: audio.TargetRate, Channels: audio.TargetChans,
		PPM: idx.PPM, DamagePacket: -1, LocatedPacket: -1,
	}
	gain, lufs, err := render(cfg, idx, inputDigest, cpStore, rep)
	if err != nil {
		return nil, err
	}
	rep.LUFS = lufs
	_ = gain
	return rep, nil
}

func loadOrScan(st *index.Store, inputPath, digest string, params index.Params) (*index.Index, error) {
	if loaded, err := st.Load(); err == nil && st.Valid(loaded, digest, params) {
		return loaded, nil
	}
	f, err := os.Open(inputPath)
	if err != nil {
		return nil, &media.IOError{Op: "open", Path: inputPath, Reason: err.Error()}
	}
	defer f.Close()
	idx, err := index.Scan(f, params, nil)
	if err != nil {
		return nil, err
	}
	if err := st.Save(idx); err != nil {
		return nil, err
	}
	return idx, nil
}

func mapScanError(err error) error {
	if f, ok := media.FindFault(err); ok && f.Kind == media.FaultFraming {
		return fmt.Errorf("%w: %v", errDamaged, err)
	}
	return err
}

// packetReader 按索引条目的字节偏移随机读取 RTP 数据报，
// 使续跑能从任意条目开始，而不必重读整个文件。
type packetReader struct {
	f   *os.File
	br  *rtp.Reader
	buf []byte
}

func newPacketReader(f *os.File) *packetReader {
	return &packetReader{f: f, buf: make([]byte, 0, 4096)}
}

// payloadAt 定位到条目偏移，读取单条数据报并返回其负载。
func (p *packetReader) payloadAt(e index.Entry) ([]byte, error) {
	if _, err := p.f.Seek(e.Offset, io.SeekStart); err != nil {
		return nil, &media.IOError{Op: "seek", Reason: err.Error()}
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(p.f, lenBuf[:]); err != nil {
		return nil, err
	}
	n := int(binaryBE(lenBuf[:]))
	if cap(p.buf) < n {
		p.buf = make([]byte, n)
	}
	p.buf = p.buf[:n]
	if _, err := io.ReadFull(p.f, p.buf); err != nil {
		return nil, err
	}
	dg, err := rtp.ParseDatagram(p.buf)
	if err != nil {
		return nil, err
	}
	return dg.Payload, nil
}
