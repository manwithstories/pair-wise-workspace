package demux

import (
	"encoding/binary"
	"io"
	"os"

	"avpipe/internal/media"
	"avpipe/internal/metadata"
)

// containerFacts is the display information declared by the container
// wrapper itself (the MP4 track header). The codec/bitstream path is what
// FFmpeg exposes through codec parameters; keeping them separate lets the
// metadata resolver report a container-versus-bitstream conflict instead of
// hiding it.
type containerFacts struct {
	// DeclaredWidth/Height come from tkhd. Zero means the container did not
	// declare a display size.
	DeclaredWidth  int
	DeclaredHeight int
	// DeclaredDegrees is the rotation encoded in the track header matrix, when
	// the container carries one. HasRotation distinguishes 0 from "absent".
	DeclaredDegrees int
	HasRotation     bool
	// Offset is the byte offset of the tkhd box, for error tracing.
	Offset int64
}

// readMP4ContainerFacts parses tkhd boxes from an MP4/MOV file. It walks the
// box tree without seeking past the file, never allocates unbounded buffers
// and rejects boxes whose declared size escapes the file.
func readMP4ContainerFacts(path string, maxBytes int64) ([]containerFacts, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, media.New(media.KindInternal, media.Origin{File: path}, "demux.readMP4ContainerFacts", "open failed", err)
	}
	defer f.Close()
	data := make([]byte, 0, 1<<20)
	// Read the whole moov payload lazily but bounded by maxBytes.
	buf := make([]byte, 64<<10)
	var total int64
	for {
		n, err := f.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > maxBytes {
				return nil, media.Errorf(media.KindUsage, media.Origin{File: path},
					"demux.readMP4ContainerFacts", "container exceeds limit %d bytes", maxBytes)
			}
			data = append(data, buf[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, media.New(media.KindInternal, media.Origin{File: path},
				"demux.readMP4ContainerFacts", "read failed", err)
		}
	}
	var facts []containerFacts
	if err := walkBoxes(data, 0, int64(len(data)), path, &facts); err != nil {
		return nil, err
	}
	return facts, nil
}

// box is a decoded box header.
type box struct {
	typ        string
	payloadOff int64
	payloadLen int64
	off        int64
}

// walkBoxes iterates the boxes in data[off:end] and collects tkhd facts from
// any trak it finds. Unknown boxes are skipped, malformed ones are reported.
func walkBoxes(data []byte, off, end int64, path string, facts *[]containerFacts) error {
	for off+8 <= end {
		size := int64(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		hdr := int64(8)
		if size == 1 {
			if off+16 > end {
				return media.Errorf(media.KindCorrupt, media.Origin{File: path, Offset: off},
					"demux.walkBoxes", "large-size box header truncated")
			}
			size = int64(binary.BigEndian.Uint64(data[off+8:]))
			hdr = 16
		} else if size == 0 {
			// Box extends to end of file.
			size = end - off
		}
		if size < hdr || off+size > end {
			return media.Errorf(media.KindCorrupt, media.Origin{File: path, Offset: off},
				"demux.walkBoxes", "box %q declares size %d beyond container", typ, size)
		}
		pOff := off + hdr
		switch typ {
		case "moov", "trak", "mdia":
			if err := walkBoxes(data, pOff, off+size, path, facts); err != nil {
				return err
			}
		case "tkhd":
			cf, err := parseTkhd(data[pOff:off+size], pOff, path)
			if err != nil {
				return err
			}
			*facts = append(*facts, cf)
		}
		off += size
	}
	return nil
}

// parseTkhd extracts the 16.16 fixed-point display size and the rotation
// encoded in the track header matrix.
func parseTkhd(payload []byte, base int64, path string) (containerFacts, error) {
	if len(payload) < 84 {
		return containerFacts{}, media.Errorf(media.KindCorrupt, media.Origin{File: path, Offset: base},
			"demux.parseTkhd", "tkhd payload too short (%d bytes)", len(payload))
	}
	version := payload[0]
	var whOff, matrixOff int
	if version == 0 {
		// version+flags(4) ctime(4) mtime(4) trackid(4) reserved(4) dur(4)
		// reserved(8) layer(2) alt(2) vol(2) reserved(2) matrix(36) wh(8)
		matrixOff = 4 + 4 + 4 + 4 + 4 + 4 + 8 + 2 + 2 + 2 + 2
		whOff = matrixOff + 36
	} else if version == 1 {
		matrixOff = 4 + 8 + 8 + 4 + 4 + 8 + 8 + 2 + 2 + 2 + 2
		whOff = matrixOff + 36
	} else {
		return containerFacts{}, media.Errorf(media.KindCorrupt, media.Origin{File: path, Offset: base},
			"demux.parseTkhd", "unsupported tkhd version %d", version)
	}
	if whOff+8 > len(payload) {
		return containerFacts{}, media.Errorf(media.KindCorrupt, media.Origin{File: path, Offset: base},
			"demux.parseTkhd", "tkhd width/height fields truncated")
	}
	cf := containerFacts{
		DeclaredWidth:  int(binary.BigEndian.Uint32(payload[whOff:]) >> 16),
		DeclaredHeight: int(binary.BigEndian.Uint32(payload[whOff+4:]) >> 16),
		Offset:         base,
	}
	// The matrix is nine 16.16 fixed-point values stored row-major:
	//   [ a b u
	//     c d v
	//     x y w ]
	// so a=+0, b=+4, c=+12, d=+16. Compare raw integers so identity
	// (a=d=65536, b=c=0) is detected exactly without float rounding.
	a := int32(binary.BigEndian.Uint32(payload[matrixOff:]))
	b := int32(binary.BigEndian.Uint32(payload[matrixOff+4:]))
	c := int32(binary.BigEndian.Uint32(payload[matrixOff+12:]))
	d := int32(binary.BigEndian.Uint32(payload[matrixOff+16:]))
	if !(a == 65536 && b == 0 && c == 0 && d == 65536) {
		cf.DeclaredDegrees = matrixDegrees(a, b, c, d)
		cf.HasRotation = true
	}
	return cf, nil
}

// matrixDegrees converts a normalised 2x2 display matrix into a clockwise
// rotation in degrees. tkhd matrices only ever contain 0, ±1 in a,b,c,d, so
// each right angle maps exactly.
func matrixDegrees(a, b, c, d int32) int {
	switch {
	case a == 0 && b > 0 && c < 0 && d == 0:
		return 90
	case a < 0 && b == 0 && c == 0 && d < 0:
		return 180
	case a == 0 && b < 0 && c > 0 && d == 0:
		return 270
	case a == 65536 && b == 0 && c == 0 && d == 65536:
		return 0
	default:
		// Non-right-angle or sheared matrix: report a 45 placeholder so the
		// resolver rejects it as an illegal rotation instead of silently
		// treating it as upright.
		return 45
	}
}

// attachContainerFacts merges the container's declared display size into the
// track facts. A container-declared size that does not match the coded frame
// becomes a crop candidate resolved by metadata precedence, and rotation from
// the container is a lower-priority candidate than the track's side data.
func attachContainerFacts(f *metadata.VideoFacts, cf containerFacts) {
	if cf.DeclaredWidth > 0 && cf.DeclaredHeight > 0 &&
		(cf.DeclaredWidth != f.Width || cf.DeclaredHeight != f.Height) {
		f.ContainerCrop = &media.CropRect{W: cf.DeclaredWidth, H: cf.DeclaredHeight, X: 0, Y: 0}
	}
	if cf.HasRotation {
		f.HasContainerRot = true
		f.ContainerRotation = cf.DeclaredDegrees
	}
}
