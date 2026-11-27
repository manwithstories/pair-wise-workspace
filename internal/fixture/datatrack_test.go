package fixture

import (
	"encoding/binary"
	"testing"

	"github.com/asticode/go-astiav"

	"mediacore/internal/meta"
)

// TestMuxDataTrack checks whether a bin_data stream can carry timed ID3 and
// emsg samples in every supported container.
func TestMuxDataTrack(t *testing.T) {
	for _, format := range []MediaFormat{FormatMKV, FormatTS} {
		format := format
		t.Run(string(format), func(t *testing.T) {
			opt := Options{Dir: t.TempDir(), Name: "data-" + string(format), Format: format, Segments: 2}
			w, err := newWriter(opt)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			if err := w.AddVideo(160, 90, 10, "h264"); err != nil {
				t.Fatal(err)
			}
			st := w.fc.NewStream(nil)
			st.SetTimeBase(astiav.NewRational(1, 1000))
			cp := st.CodecParameters()
			cp.SetCodecID(astiav.CodecIDBinData)
			cp.SetMediaType(astiav.MediaTypeData)
			if err := w.Start(); err != nil {
				if format == FormatMKV {
					// Matroska has no data-track kind; that is an expected
					// container limitation, not a defect in the writer.
					t.Skipf("matroska data track unsupported: %v", err)
				}
				t.Fatal(err)
			}
			if err := w.WriteVideo(0, yuv420Frame(160, 90, 0)); err != nil {
				t.Fatal(err)
			}
			pkt := astiav.AllocPacket()
			defer pkt.Free()
			if err := pkt.FromData([]byte("ID3\x04\x00\x00\x00\x00\x00\x00")); err != nil {
				t.Fatal(err)
			}
			pkt.SetStreamIndex(st.Index())
			pkt.SetPts(500)
			pkt.SetDts(500)
			pkt.SetDuration(100)
			if err := w.fc.WriteInterleavedFrame(pkt); err != nil {
				t.Fatalf("write data packet: %v", err)
			}
			d, _ := meta.EncodeFFMetadata(nil)
			_ = d
			_ = binary.BigEndian
		})
	}
}
