package slicer

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"tsprobe/internal/tsio"
)

// WriteSegments writes each planned slice by streaming the source a second time.
// Only one input and one output buffer are held in memory at once.
func WriteSegments(input string, idx *Index) error {
	f, err := os.Open(input)
	if err != nil {
		return err
	}
	defer f.Close()
	r := tsio.New(f)
	var current *os.File
	var bw *bufio.Writer
	sliceIdx := 0
	packet := make([]byte, tsio.PacketSize)
	closeCurrent := func() error {
		if bw != nil {
			if err := bw.Flush(); err != nil {
				return err
			}
			bw = nil
		}
		if current != nil {
			if err := current.Close(); err != nil {
				return err
			}
			current = nil
		}
		return nil
	}
	defer closeCurrent()
	for sliceIdx < len(idx.Slices.Items) {
		_, err := io.ReadFull(r, packet)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return err
		}
		packetIndex := r.PacketIndex()
		for sliceIdx < len(idx.Slices.Items) && packetIndex > idx.Slices.Items[sliceIdx].EndPacket {
			if err := closeCurrent(); err != nil {
				return err
			}
			sliceIdx++
		}
		if sliceIdx >= len(idx.Slices.Items) {
			break
		}
		it := &idx.Slices.Items[sliceIdx]
		if packetIndex < it.StartPacket {
			continue
		}
		if current == nil {
			if err := os.MkdirAll(filepath.Dir(it.File), 0o755); err != nil {
				return err
			}
			current, err = os.Create(it.File)
			if err != nil {
				return err
			}
			bw = bufio.NewWriterSize(current, 128*1024)
		}
		if _, err := bw.Write(packet); err != nil {
			return err
		}
		if it.WrittenPackets == 0 {
			it.StartOffset = r.PacketOffset()
		}
		it.EndOffset = r.PacketOffset() + tsio.PacketSize
		it.WrittenPackets++
	}
	if err := closeCurrent(); err != nil {
		return err
	}
	for i := range idx.Slices.Items {
		if idx.Slices.Items[i].WrittenPackets == 0 {
			return fmt.Errorf("slice %d contains no TS packets", i+1)
		}
	}
	return nil
}
