package index

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"medpipe/internal/clock"
)

// writeWAV 生成规范 PCM WAV；dataLen 为写入 data 块的声明长度，
// actualDataLen 控制实际落盘字节，用于构造截断与半帧样例。
func writeWAV(t *testing.T, path string, rate int, channels, bits int, samples []byte, declared int) {
	t.Helper()
	align := channels * bits / 8
	var buf bytes.Buffer
	dataLen := int64(len(samples))
	if declared >= 0 {
		dataLen = int64(declared)
	}
	riffLen := 4 + (8 + 16) + (8 + dataLen)
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(riffLen))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(channels))
	binary.Write(&buf, binary.LittleEndian, uint32(rate))
	binary.Write(&buf, binary.LittleEndian, uint32(uint32(rate)*uint32(align)))
	binary.Write(&buf, binary.LittleEndian, uint16(align))
	binary.Write(&buf, binary.LittleEndian, uint16(bits))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(dataLen))
	buf.Write(samples)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("写夹具失败: %v", err)
	}
}

func TestScanValidWAV(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "block_000000.wav")
	// 2 声道 16bit，4 个采样帧 = 16 字节
	samples := make([]byte, 16)
	for i := range samples {
		samples[i] = byte(i + 1)
	}
	writeWAV(t, p, 48000, 2, 16, samples, 16)

	bi, err := ScanBlock(0, p, DefaultMaxBlock)
	if err != nil {
		t.Fatalf("ScanBlock: %v", err)
	}
	if bi.SampleRate != 48000 || bi.Channels != 2 || bi.BitsPerSample != 16 {
		t.Fatalf("头字段错误: %+v", bi)
	}
	if bi.Frames != 4 || bi.DataBytes != 16 || bi.Truncated {
		t.Fatalf("帧/数据统计错误: %+v", bi)
	}
	if bi.SHA256 == "" || len(bi.SHA256) != 64 {
		t.Fatal("SHA-256 摘要缺失")
	}
	// 确定性：重复扫描摘要一致
	bi2, _ := ScanBlock(0, p, DefaultMaxBlock)
	if bi2.SHA256 != bi.SHA256 {
		t.Fatal("同一文件摘要不稳定")
	}
}

func TestScanTruncation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "block_000003.wav")
	// 声明 16 字节（8 帧，16bit 单声道），实际只落 8 字节（4 帧）
	writeWAV(t, p, 48000, 1, 16, make([]byte, 8), 16)
	bi, err := ScanBlock(3, p, DefaultMaxBlock)
	if err != nil {
		t.Fatalf("完整采样边界截断应接受: %v", err)
	}
	if !bi.Truncated || bi.Frames != 4 || bi.MissingFrames != 4 {
		t.Fatalf("截断统计错误: %+v", bi)
	}

	// 半采样帧截断必须拒绝
	ph := filepath.Join(dir, "block_000004.wav")
	writeWAV(t, ph, 48000, 1, 16, []byte{1, 2, 3}, 16)
	if _, err := ScanBlock(4, ph, DefaultMaxBlock); clock.CodeOf(err) != clock.CodeInvalidHeader {
		t.Fatalf("半帧截断应报 invalid_header, 得 %v", err)
	}
}

func TestScanRejectBadInput(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "block_000001.wav")
	if err := os.WriteFile(bad, []byte("NOTAWAV......"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanBlock(1, bad, DefaultMaxBlock); clock.CodeOf(err) != clock.CodeInvalidHeader {
		t.Fatalf("非法头应报 invalid_header, 得 %v", err)
	}

	// 超限：数据段声明超过 1 KiB 上限
	big := filepath.Join(dir, "block_000002.wav")
	writeWAV(t, big, 48000, 1, 16, make([]byte, 16), 4096)
	if _, err := ScanBlock(2, big, 1024); clock.CodeOf(err) != clock.CodeOversize {
		t.Fatalf("超限应报 oversize, 得 %v", err)
	}

	// 非法参数：3 声道 16bit 的 blockAlign 应为 6，故意写 4
	fp := filepath.Join(dir, "block_000005.wav")
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(52))
	buf.WriteString("WAVEfmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(3))
	binary.Write(&buf, binary.LittleEndian, uint32(48000))
	binary.Write(&buf, binary.LittleEndian, uint32(48000*4))
	binary.Write(&buf, binary.LittleEndian, uint16(4))
	binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(12))
	buf.Write(make([]byte, 12))
	os.WriteFile(fp, buf.Bytes(), 0o644)
	if _, err := ScanBlock(5, fp, DefaultMaxBlock); clock.CodeOf(err) != clock.CodeInvalidHeader {
		t.Fatalf("blockAlign 不符应拒绝, 得 %v", err)
	}
}

func TestSafeJoin(t *testing.T) {
	base := t.TempDir()
	cases := map[string]bool{
		"block_000000.wav": true,
		"a/../../etc/passwd": false,
		"../secret.wav":      false,
		"/etc/passwd":        false,
		"./sub/block.wav":    true,
	}
	for name, ok := range cases {
		_, err := SafeJoin(base, name)
		if ok && err != nil {
			t.Fatalf("%s 应通过, 得 %v", name, err)
		}
		if !ok && clock.CodeOf(err) != clock.CodePathRejected {
			t.Fatalf("%s 应报 path_rejected, 得 %v", name, err)
		}
	}
}

func TestScanDirAndCacheRebuild(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "cache.json")
	writeWAV(t, filepath.Join(dir, "block_000000.wav"), 48000, 2, 16, make([]byte, 16), 16)
	writeWAV(t, filepath.Join(dir, "block_000001.wav"), 48000, 2, 16, make([]byte, 16), 16)

	idx, rebuilt, err := LoadOrRebuild(dir, cache, DefaultMaxBlock)
	if err != nil || rebuilt || len(idx.Blocks) != 2 {
		t.Fatalf("首次构建: rebuilt=%v err=%v n=%d", rebuilt, err, len(idx.Blocks))
	}
	if idx.Blocks[0].ID != 0 || idx.Blocks[1].ID != 1 {
		t.Fatal("块未按序号升序")
	}
	// 第二次：内容未变，必须命中缓存
	_, rebuilt, err = LoadOrRebuild(dir, cache, DefaultMaxBlock)
	if err != nil || rebuilt {
		t.Fatalf("未变化输入不应重建: rebuilt=%v err=%v", rebuilt, err)
	}
	// 篡改第一个块后应作废旧缓存并重建
	p := filepath.Join(dir, "block_000000.wav")
	raw, _ := os.ReadFile(p)
	raw[len(raw)-1] ^= 0xFF
	os.WriteFile(p, raw, 0o644)
	idx2, rebuilt, err := LoadOrRebuild(dir, cache, DefaultMaxBlock)
	if err != nil || !rebuilt {
		t.Fatalf("内容变化应触发重建: rebuilt=%v err=%v", rebuilt, err)
	}
	if idx2.Blocks[0].SHA256 == idx.Blocks[0].SHA256 {
		t.Fatal("重建后摘要未更新")
	}
}

func TestBlockName(t *testing.T) {
	dir := t.TempDir()
	writeWAV(t, filepath.Join(dir, "block_000007.wav"), 48000, 1, 16, make([]byte, 4), 4)
	idx, err := ScanDir(dir, DefaultMaxBlock)
	if err != nil {
		t.Fatalf("ScanDir: %v", err)
	}
	if len(idx.Blocks) != 1 || idx.Blocks[0].ID != 7 {
		t.Fatalf("块号解析失败: %+v", idx.Blocks)
	}
}
