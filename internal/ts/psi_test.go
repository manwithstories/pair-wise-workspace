package ts

import (
	"testing"

	"mediatool/internal/clock"
)

// buildSection 构造带正确 CRC 的 PSI 表段。
func buildSection(tableID byte, version int, body []byte) []byte {
	secLen := 5 + len(body) + 4 // 固定头 5 字节 + body + CRC
	raw := make([]byte, 3, 3+secLen)
	raw[0] = tableID
	raw[1] = 0xB0 | byte(secLen>>8)
	raw[2] = byte(secLen)
	raw = append(raw, 0x00, 0x01, 0xC1|byte(version<<1), 0x00, 0x00)
	raw = append(raw, body...)
	crc := CRC32MPEG(raw)
	return append(raw, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}

func patBody(pmtPID uint16) []byte {
	return []byte{0x00, 0x01, 0xE0 | byte(pmtPID>>8), byte(pmtPID)}
}

func pmtBody(entries []PMTEntry, padTo int) []byte {
	body := []byte{0xE1, 0x01, 0xF0, 0x00} // PCR PID 0x101, program_info_length 0
	for _, e := range entries {
		body = append(body, e.StreamType, 0xE0|byte(e.PID>>8), byte(e.PID), 0xF0, 0x00)
	}
	for len(body) < padTo {
		body = append(body, 0xFF)
	}
	return body
}

// packetizeSection 将表段切成 TS 包（首包含 pointer_field=0）。
func packetizeSection(pid uint16, sec []byte, ccStart uint8) [][]byte {
	var pkts [][]byte
	first := true
	cc := ccStart
	for len(sec) > 0 {
		pkt := make([]byte, PacketSize)
		pkt[0] = SyncByte
		pkt[1] = byte(pid >> 8)
		pkt[2] = byte(pid)
		pkt[3] = 0x10 | cc&0x0F
		pos := 4
		if first {
			pkt[1] |= 0x40
			pkt[4] = 0x00
			pos = 5
		}
		n := copy(pkt[pos:], sec)
		sec = sec[n:]
		for i := pos + n; i < PacketSize; i++ {
			pkt[i] = 0xFF
		}
		pkts = append(pkts, pkt)
		first = false
		cc++
	}
	return pkts
}

func feedAll(a *PSIAssembler, pkts [][]byte) (secs []Section, faults clock.Faults) {
	for _, raw := range pkts {
		p, err := parsePacket(raw, 0)
		if err != nil {
			panic(err)
		}
		s, f := a.Feed(p)
		secs = append(secs, s...)
		faults = append(faults, f...)
	}
	return secs, faults
}

func TestPSIAssemblyAndVersionSwitch(t *testing.T) {
	a := NewPSIAssembler()
	pat := packetizeSection(0, buildSection(0x00, 0, patBody(0x100)), 0)
	pmtV0 := packetizeSection(0x100, buildSection(0x02, 0,
		pmtBody([]PMTEntry{{StreamType: 0x1B, PID: 0x101}}, 0)), 0)
	pmtV1 := packetizeSection(0x100, buildSection(0x02, 1,
		pmtBody([]PMTEntry{{StreamType: 0x1B, PID: 0x101}, {StreamType: 0x0F, PID: 0x102}}, 0)), 1)

	var faults clock.Faults
	_, f1 := feedAll(a, pat)
	_, f2 := feedAll(a, pmtV0)
	_, f3 := feedAll(a, pmtV1)
	faults = append(append(append(faults, f1...), f2...), f3...)

	if len(faults) != 1 || faults[0].Kind != clock.FaultPSIVersion {
		t.Fatalf("want one psi-version fault, got %v", faults)
	}
	pmt, ok := a.LastValidPMT(0x100)
	if !ok {
		t.Fatal("no valid PMT")
	}
	if pmt.Version != 1 || len(pmt.Entries) != 2 {
		t.Fatalf("PMT = %+v, want version 1 with 2 entries", pmt)
	}
}

func TestPSISpanningPackets(t *testing.T) {
	a := NewPSIAssembler()
	feedAll(a, packetizeSection(0, buildSection(0x00, 0, patBody(0x100)), 0))
	// 填充描述符使表段跨 3 个包。
	big := buildSection(0x02, 3, pmtBody([]PMTEntry{{StreamType: 0x1B, PID: 0x101}}, 400))
	pkts := packetizeSection(0x100, big, 0)
	if len(pkts) < 3 {
		t.Fatalf("section spans %d packets, want >=3", len(pkts))
	}
	secs, faults := feedAll(a, pkts)
	if len(faults) != 0 {
		t.Fatalf("faults: %v", faults)
	}
	if len(secs) != 1 || secs[0].Version != 3 {
		t.Fatalf("sections = %+v", secs)
	}
}

func TestTruncatedPSIKeepsLastValid(t *testing.T) {
	a := NewPSIAssembler()
	feedAll(a, packetizeSection(0, buildSection(0x00, 0, patBody(0x100)), 0))
	good := buildSection(0x02, 0, pmtBody([]PMTEntry{{StreamType: 0x1B, PID: 0x101}}, 0))
	feedAll(a, packetizeSection(0x100, good, 0))

	// 新版本表段跨 2 包，第二包丢失 → Flush 报不完整，旧映射保留。
	big := buildSection(0x02, 1, pmtBody([]PMTEntry{{StreamType: 0x1B, PID: 0x101}}, 300))
	pkts := packetizeSection(0x100, big, 1)
	if len(pkts) < 2 {
		t.Fatal("need multi-packet section")
	}
	feedAll(a, pkts[:1])
	faults := a.Flush()
	if len(faults) != 1 || faults[0].Kind != clock.FaultPSIIncomplete {
		t.Fatalf("flush faults = %v", faults)
	}
	pmt, ok := a.LastValidPMT(0x100)
	if !ok || pmt.Version != 0 {
		t.Fatalf("last valid PMT = %+v ok=%v, want version 0", pmt, ok)
	}
}

func TestCorruptCRCRejected(t *testing.T) {
	a := NewPSIAssembler()
	feedAll(a, packetizeSection(0, buildSection(0x00, 0, patBody(0x100)), 0))
	bad := buildSection(0x02, 0, pmtBody([]PMTEntry{{StreamType: 0x1B, PID: 0x101}}, 0))
	bad[len(bad)-1] ^= 0xFF // 破坏 CRC
	_, faults := feedAll(a, packetizeSection(0x100, bad, 0))
	if len(faults) != 1 || faults[0].Kind != clock.FaultPSIIncomplete {
		t.Fatalf("faults = %v", faults)
	}
	if _, ok := a.LastValidPMT(0x100); ok {
		t.Fatal("corrupt PMT must not become last valid")
	}
}
