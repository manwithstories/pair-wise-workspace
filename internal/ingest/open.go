package ingest

import (
	"io"
	"os"

	"github.com/abema/go-mp4"

	"mediarun/internal/mediaerr"
)

// Open sniffs the container by its leading magic bytes and returns the
// matching streamer. An input that is neither an ISOBMFF ftyp file nor the
// documented MXF OP1a partition is rejected as UNKNOWN_STRUCTURE rather than
// guessed from its extension.
func Open(path string) (Streamer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(path)
	}
	head := make([]byte, 16)
	n, _ := io.ReadFull(f, head)
	_ = f.Close()
	if n >= 8 {
		// ISOBMFF: 4-byte size followed by the 'ftyp' brand.
		if string(head[4:8]) == mp4.BoxTypeFtyp().String() {
			return OpenMP4(path)
		}
	}
	if n >= 16 {
		var ul [16]byte
		copy(ul[:], head[:16])
		if ul == mxfPartitionUL {
			return OpenMXF(path)
		}
	}
	return nil, mediaerr.New(mediaerr.CodeUnknownStructure,
		"input is neither MP4 (ftyp) nor the MXF OP1a subset").
		At("", "", 0).OnPath(path)
}
