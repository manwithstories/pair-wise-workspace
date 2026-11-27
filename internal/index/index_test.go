package index

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"relay/internal/media"
)

func frame(b []byte) []byte {
	out := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	copy(out[4:], b)
	return out
}

func audioPkt(seq uint16, ts uint32, payload []byte) []byte {
	h := make([]byte, 12)
	h[0] = 0x80
	h[1] = 97
	binary.BigEndian.PutUint16(h[2:4], seq)
	binary.BigEndian.PutUint32(h[4:8], ts)
	return append(h, payload...)
}

func srPkt(ntpSec, rtpTS uint32) []byte {
	b := make([]byte, 28)
	b[0] = 0x80
	b[1] = 200
	binary.BigEndian.PutUint16(b[2:4], 6)
	binary.BigEndian.PutUint32(b[8:12], ntpSec)
	binary.BigEndian.PutUint32(b[16:20], rtpTS)
	return b
}

func sampleParams() Params {
	return Params{AudioRate: 48000, Channels: 2, PayloadType: 97,
		ClockRate: 48000, TargetRate: 48000, TargetChans: 2}
}

func buildStream(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	for i := 0; i < 20; i++ {
		ts := uint32(i * 480) // 每包 10ms @48k，16bit 立体声 480 字节
		buf.Write(frame(audioPkt(uint16(i), ts, make([]byte, 480))))
		if i == 9 {
			buf.Write(frame(srPkt(2208988800+10, ts)))
		}
	}
	return buf.Bytes()
}

func TestScanBuildsEntries(t *testing.T) {
	stream := buildStream(t)
	var faults []*media.Fault
	idx, err := Scan(bytes.NewReader(stream), sampleParams(), func(f *media.Fault) {
		faults = append(faults, f)
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if idx.PacketCount != 20 || idx.SRCount != 1 {
		t.Fatalf("counts=%+v", idx)
	}
	if len(idx.Entries) != 20 {
		t.Fatalf("entries=%d", len(idx.Entries))
	}
	// 偏移精确：第 n 条记录的偏移等于此前所有 (4+长度) 之和。
	if idx.Entries[0].Offset != 0 {
		t.Fatalf("first offset=%d", idx.Entries[0].Offset)
	}
	wantOff1 := int64(4 + 12 + 480)
	if idx.Entries[1].Offset != wantOff1 {
		t.Fatalf("second offset=%d want %d", idx.Entries[1].Offset, wantOff1)
	}
	// 扩展时间戳单调，纳秒时间轴单调。
	for i := 1; i < len(idx.Entries); i++ {
		if idx.Entries[i].ExtTS <= idx.Entries[i-1].ExtTS {
			t.Fatalf("ext ts not monotonic at %d", i)
		}
		if idx.Entries[i].Nanos <= idx.Entries[i-1].Nanos {
			t.Fatalf("nanos not monotonic at %d", i)
		}
	}
	if idx.InputDigest == "" || len(idx.InputDigest) != 64 {
		t.Fatalf("bad input digest %q", idx.InputDigest)
	}
	if len(faults) != 0 {
		t.Fatalf("unexpected faults: %+v", faults)
	}
}

func TestStoreRoundTripAndAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "idx.json")
	st := NewStore(path)
	if _, err := st.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	idx, err := Scan(bytes.NewReader(buildStream(t)), sampleParams(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(idx); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 原子写不得遗留临时文件。
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
	got, err := st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.InputDigest != idx.InputDigest || got.PacketCount != idx.PacketCount ||
		len(got.Entries) != len(idx.Entries) || got.Entries[7].Seq != idx.Entries[7].Seq {
		t.Fatal("round trip mismatch")
	}
}

func TestInvalidationOnInputAndParams(t *testing.T) {
	dir := t.TempDir()
	inPath := filepath.Join(dir, "input.bin")
	stream := buildStream(t)
	if err := os.WriteFile(inPath, stream, 0o644); err != nil {
		t.Fatal(err)
	}
	st := NewStore(filepath.Join(dir, "idx.json"))
	params := sampleParams()
	idx, err := Scan(bytes.NewReader(stream), params, nil)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := InputDigest(inPath)
	if err != nil || digest != idx.InputDigest {
		t.Fatalf("file digest mismatch: %s vs %s err=%v", digest, idx.InputDigest, err)
	}
	if err := st.Save(idx); err != nil {
		t.Fatal(err)
	}
	loaded, _ := st.Load()
	if !st.Valid(loaded, digest, params) {
		t.Fatal("fresh index must be valid")
	}
	// 1) 参数变化即失效。
	changed := params
	changed.Channels = 6
	if st.Valid(loaded, digest, changed) {
		t.Fatal("param change must invalidate index")
	}
	// 2) 输入被修改后摘要变化即失效。
	stream[len(stream)-1] ^= 0xFF
	if err := os.WriteFile(inPath, stream, 0o644); err != nil {
		t.Fatal(err)
	}
	newDigest, err := InputDigest(inPath)
	if err != nil {
		t.Fatal(err)
	}
	if newDigest == digest {
		t.Fatal("digest should change after mutation")
	}
	if st.Valid(loaded, newDigest, params) {
		t.Fatal("input mutation must invalidate index")
	}
}

func TestScanSurvivesRecoverableFault(t *testing.T) {
	// 构造一个坏版本号的数据报夹在正常包之间，扫描应上抛故障并继续。
	var buf bytes.Buffer
	buf.Write(frame(audioPkt(0, 0, make([]byte, 16))))
	bad := audioPkt(1, 480, make([]byte, 16))
	bad[0] = 0 // 版本号非法
	buf.Write(frame(bad))
	buf.Write(frame(audioPkt(2, 960, make([]byte, 16))))
	var kinds []media.FaultKind
	idx, err := Scan(&buf, sampleParams(), func(f *media.Fault) { kinds = append(kinds, f.Kind) })
	if err != nil {
		t.Fatalf("recoverable fault must not abort scan: %v", err)
	}
	if idx.PacketCount != 2 || len(kinds) != 1 || kinds[0] != media.FaultBadPayload {
		t.Fatalf("count=%d kinds=%v", idx.PacketCount, kinds)
	}
}

func TestScanAbortsOnFramingFault(t *testing.T) {
	// 非法长度前缀无法定位下一条，必须返回错误供调用方按退出码 2 处理。
	stream := append(frame(audioPkt(0, 0, make([]byte, 16))), 0, 0, 0, 0)
	_, err := Scan(bytes.NewReader(stream), sampleParams(), nil)
	if f, ok := media.FindFault(err); !ok || f.Kind != media.FaultFraming {
		t.Fatalf("want framing fault, got %v", err)
	}
}
