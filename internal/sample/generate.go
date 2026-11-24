// Package sample generates deterministic local RTP/RTCP capture fixtures,
// including sequence wrap, reordering, duplicates, loss, CRC damage, truncation
// and invalid indexes. No external fixture files are used.
package sample

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"

	"mediad/internal/capture"
)

type Options struct {
	Seed     int64  `json:"seed"`
	Seconds  int    `json:"seconds"`
	Rate     int    `json:"rate"`
	Channels int    `json:"channels"`
	Dir      string `json:"dir"`
	Faults   bool   `json:"faults"`
}

type Manifest struct {
	Options
	CapturePath         string   `json:"capture_path"`
	IndexPath           string   `json:"index_path"`
	BadIndexPath        string   `json:"bad_index_path,omitempty"`
	Records             int      `json:"records"`
	RTCPRecords         int      `json:"rtcp_records"`
	RTPPackets          int      `json:"rtp_packets"`
	UniquePackets       int      `json:"unique_packets"`
	DuplicateInjections int      `json:"duplicate_injections"`
	ReorderInjections   int      `json:"reorder_injections"`
	LostInjections      int      `json:"lost_injections"`
	LostSequences       []uint16 `json:"lost_sequences"`
	BadCRCOffset        int64    `json:"bad_crc_offset"`
	TruncatedOffset     int64    `json:"truncated_offset"`
	PacketFrames        int      `json:"packet_frames"`
}

type writeItem struct {
	kind    uint8
	data    []byte
	corrupt bool
}

func DefaultOptions(seed int64, seconds int, dir string, faults bool) Options {
	return Options{Seed: seed, Seconds: seconds, Rate: 44100, Channels: 6, Dir: dir, Faults: faults}
}

func Generate(opts Options) (*Manifest, error) {
	if opts.Seconds <= 0 || opts.Seconds > 600 {
		return nil, fmt.Errorf("invalid seconds %d", opts.Seconds)
	}
	if opts.Rate == 0 {
		opts.Rate = 44100
	}
	if opts.Channels == 0 {
		opts.Channels = 6
	}
	if opts.Dir == "" {
		return nil, fmt.Errorf("sample directory is required")
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, err
	}

	packetFrames := opts.Rate / 100 // 10 ms
	packetCount := opts.Seconds * 100
	startSeq := uint16(65520)
	startTS := uint32(math.MaxUint32 - 2000)
	ssrc := uint32(0x29000000 + opts.Seed%256)

	packets := make([][]byte, packetCount)
	sequences := make([]uint16, packetCount)
	for i := 0; i < packetCount; i++ {
		seq := startSeq + uint16(i)
		ts := startTS + uint32(i*packetFrames)
		pkt := rtp.Packet{
			Header: rtp.Header{
				Version: 2, Padding: false, Extension: false,
				PayloadType: 97, SequenceNumber: seq, Timestamp: ts, SSRC: ssrc,
			},
			Payload: encodeL16(i, packetFrames, opts.Channels, opts.Rate, opts.Seed, opts.Seconds),
		}
		b, err := pkt.Marshal()
		if err != nil {
			return nil, err
		}
		packets[i] = b
		sequences[i] = seq
	}

	lost := map[int]bool{}
	swapPositions := []int{}
	duplicatePositions := []int{}
	badCRCIndex := 42
	if opts.Faults {
		for _, idx := range []int{120, 777} {
			if idx < packetCount {
				lost[idx] = true
			}
		}
		swapPositions = []int{50, 200, 400, 600, 850}
		duplicatePositions = []int{201, 621, 1051}
	}

	order := make([]int, 0, packetCount-len(lost))
	lostSeqs := []uint16{}
	for i := 0; i < packetCount; i++ {
		if lost[i] {
			lostSeqs = append(lostSeqs, sequences[i])
			continue
		}
		order = append(order, i)
	}
	for _, p := range swapPositions {
		if p+1 < len(order) {
			order[p], order[p+1] = order[p+1], order[p]
		}
	}

	items := []writeItem{}
	sr1, _ := (&rtcp.SenderReport{SSRC: ssrc, NTPTime: 0x0001020304050607, RTPTime: startTS, PacketCount: uint32(packetCount), OctetCount: uint32(len(packets[0]) * packetCount)}).Marshal()
	items = append(items, writeItem{kind: capture.KindRTCP, data: sr1})
	for _, idx := range order {
		items = append(items, writeItem{kind: capture.KindRTP, data: packets[idx]})
	}
	sr2, _ := (&rtcp.SenderReport{SSRC: ssrc, NTPTime: 0x0002030405060708, RTPTime: startTS + uint32(600*packetFrames), PacketCount: 600}).Marshal()
	items = append(items, writeItem{kind: capture.KindRTCP, data: sr2})
	for _, idx := range duplicatePositions {
		if idx < len(packets) && !lost[idx] {
			items = append(items, writeItem{kind: capture.KindRTP, data: packets[idx]})
		}
	}
	badCRCOffset := int64(-1)
	if opts.Faults && badCRCIndex < len(packets) {
		items = append(items, writeItem{kind: capture.KindRTP, data: packets[badCRCIndex], corrupt: true})
	}

	var buf bytes.Buffer
	records := 0
	rtcpRecords := 0
	for _, item := range items {
		arrival := int64(records+1) * 20_000_000
		start := int64(buf.Len())
		if item.corrupt {
			// 8-byte frame header + kind + arrival + 16 bytes into RTP packet.
			badCRCOffset = start + 8 + 1 + 8 + 16
		}
		if _, err := capture.WriteRecord(&buf, item.kind, arrival, item.data); err != nil {
			return nil, err
		}
		if item.corrupt {
			b := buf.Bytes()
			if badCRCOffset < int64(len(b)) {
				b[badCRCOffset] ^= 0x01
			}
		}
		records++
		if item.kind == capture.KindRTCP {
			rtcpRecords++
		}
	}

	truncatedOffset := int64(-1)
	if opts.Faults {
		truncatedOffset = int64(buf.Len())
		buf.Write([]byte("MDR1"))
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], 96)
		buf.Write(l[:])
		buf.Write([]byte{1, 0, 0}) // partial body
	}

	capturePath := filepath.Join(opts.Dir, "input.mdr")
	if err := os.WriteFile(capturePath, buf.Bytes(), 0o644); err != nil {
		return nil, err
	}

	idx, err := capture.BuildIndex(bytes.NewReader(buf.Bytes()), int64(buf.Len()), 32)
	if err != nil {
		return nil, err
	}
	indexPath := filepath.Join(opts.Dir, "index.json")
	if err := capture.WriteIndex(indexPath, idx); err != nil {
		return nil, err
	}
	badIndexPath := ""
	if opts.Faults && len(idx.Entries) > 0 {
		bad := *idx
		bad.Entries = append([]capture.IndexEntry(nil), idx.Entries...)
		badIndexPath = filepath.Join(opts.Dir, "index-bad-oob.json")
		bad.Entries[len(bad.Entries)-1].Offset = idx.FileSize + 1<<20
		if err := capture.WriteIndex(badIndexPath, &bad); err != nil {
			return nil, err
		}
		truncatedIndexPath := filepath.Join(opts.Dir, "index-truncated.json")
		_ = os.WriteFile(truncatedIndexPath, []byte(`{"version":1,"entries":[`), 0o644)
	}

	m := &Manifest{
		Options: opts, CapturePath: capturePath, IndexPath: indexPath, BadIndexPath: badIndexPath,
		Records: records, RTCPRecords: rtcpRecords, RTPPackets: packetCount,
		UniquePackets: packetCount - len(lost), DuplicateInjections: len(duplicatePositions),
		ReorderInjections: len(swapPositions), LostInjections: len(lost), LostSequences: lostSeqs,
		BadCRCOffset: badCRCOffset, TruncatedOffset: truncatedOffset, PacketFrames: packetFrames,
	}
	if b, err := json.MarshalIndent(m, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(opts.Dir, "manifest.json"), b, 0o644)
	}
	return m, nil
}
