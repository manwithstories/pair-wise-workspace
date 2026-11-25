package ts

import (
	"fmt"

	"mediqc/internal/media"
)

const (
	SyncByte   = 0x47
	Size188    = 188
	Size204    = 204
	rsTrailer  = 16
	syncProbes = 4
)

// SyncReport describes TS packet alignment and the first failed boundary.
type SyncReport struct {
	PacketSize    int   `json:"packet_size"`
	SyncOffset    int   `json:"sync_offset"`
	FirstDesync   int64 `json:"first_desync"`
	Checked       int   `json:"checked"`
	ResyncOffset  int64 `json:"resync_offset"`
	TrailerLength int   `json:"trailer_length"`
}

// FindFirstDesync validates fixed-stride sync bytes. It returns -1 when every
// complete candidate packet begins with 0x47.
func FindFirstDesync(data []byte, packetSize int) int64 {
	if packetSize != Size188 && packetSize != Size204 {
		return 0
	}
	if len(data) == 0 || data[0] != SyncByte {
		return 0
	}
	packets := len(data) / packetSize
	for i := 1; i < packets; i++ {
		off := i * packetSize
		if data[off] != SyncByte {
			return int64(off)
		}
	}
	return -1
}

// DetectSync selects 188 or 204 byte framing and locates the first mismatch.
func DetectSync(data []byte, limits media.Limits) (SyncReport, *media.Error) {
	if len(data) == 0 {
		return SyncReport{}, &media.Error{
			Code: media.CodeUnexpectedEOF, Component: "ts.sync",
			Message: "empty transport stream",
		}
	}
	if int64(len(data)/Size188) > int64(limits.MaxPackets) {
		return SyncReport{}, &media.Error{
			Code: media.CodeResourceLimit, Component: "ts.sync",
			Message: fmt.Sprintf("packet count exceeds %d", limits.MaxPackets),
		}
	}

	first := FindFirstDesync(data, Size188)
	if first < 0 {
		return SyncReport{PacketSize: Size188, SyncOffset: 0, FirstDesync: -1, Checked: len(data) / Size188}, nil
	}
	if d204 := FindFirstDesync(data, Size204); d204 < 0 && len(data) >= Size204*syncProbes {
		return SyncReport{
			PacketSize: Size204, SyncOffset: 0, FirstDesync: -1,
			Checked: len(data) / Size204, TrailerLength: rsTrailer,
		}, nil
	}

	offset, size := FindResync(data, first)
	report := SyncReport{
		PacketSize: size, SyncOffset: int(offset), FirstDesync: first,
		ResyncOffset: offset,
	}
	if size == Size204 {
		report.TrailerLength = rsTrailer
	}
	if offset < 0 {
		return report, (&media.Error{
			Code: media.CodeSyncLost, Component: "ts.sync", Offset: first,
			Message: "no 188/204-byte sync pattern found",
		}).MarkFatal()
	}
	return report, (&media.Error{
		Code: media.CodeSyncLost, Component: "ts.sync", Offset: first,
		Message: fmt.Sprintf("resynchronized at byte %d", offset),
	}).MarkRecoverable()
}

// FindResync searches for an offset and packet size with syncProbes consecutive
// sync bytes. It bounds scanning to one packet plus a small guard region.
func FindResync(data []byte, start int64) (int64, int) {
	if start < 0 {
		start = 0
	}
	maxScan := int(start) + Size204*2
	if maxScan > len(data) {
		maxScan = len(data)
	}
	for off := int(start); off < maxScan; off++ {
		if data[off] != SyncByte {
			continue
		}
		for _, size := range []int{Size188, Size204} {
			if syncAtStride(data, off, size) {
				return int64(off), size
			}
		}
	}
	return -1, 0
}

func syncAtStride(data []byte, off, size int) bool {
	for i := 0; i < syncProbes; i++ {
		p := off + i*size
		if p >= len(data) || data[p] != SyncByte {
			return false
		}
	}
	return true
}

// PacketCount returns complete packets for the selected framing.
func PacketCount(data []byte, packetSize int) int {
	if packetSize <= 0 {
		return 0
	}
	return len(data) / packetSize
}
