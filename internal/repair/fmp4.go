package repair

import (
	"bytes"
	"encoding/binary"

	"github.com/abema/go-mp4"

	"github.com/pairwise/mediad/internal/media"
	"github.com/pairwise/mediad/internal/store"
)

// MaxFragmentBytes is the repair pipeline's per-fragment hard limit.
const MaxFragmentBytes = store.MaxSegmentBytes

func box(boxType string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(out)))
	copy(out[4:8], boxType)
	copy(out[8:], payload)
	return out
}

func fullBox(boxType string, version byte, flags uint32, payload []byte) []byte {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(boxType, append(head, payload...))
}

// BuildInitSegment creates a deterministic initialization segment. Timescale is
// encoded in an mdhd box and is also resolved through media.Timeline.
func BuildInitSegment(trackID string, timescale uint32) ([]byte, error) {
	var ftypBuf bytes.Buffer
	ftyp := &mp4.Ftyp{MajorBrand: [4]byte{'i', 's', 'o', 'm'}, MinorVersion: 0x200}
	ftyp.AddCompatibleBrand([4]byte{'i', 's', 'o', 'm'})
	ftyp.AddCompatibleBrand([4]byte{'m', 'p', '4', '1'})
	if _, err := mp4.Marshal(&ftypBuf, ftyp, mp4.Context{}); err != nil {
		return nil, media.Wrap(err, media.CodeInternal, "repair.fmp4", "ftyp", "marshal failed")
	}
	mdhdPayload := make([]byte, 20)
	binary.BigEndian.PutUint32(mdhdPayload[12:16], timescale)
	binary.BigEndian.PutUint16(mdhdPayload[16:18], 0x55c4) // undetermined language "und"
	mdhd := fullBox("mdhd", 0, 0, mdhdPayload)
	hdlr := fullBox("hdlr", 0, 0, append([]byte{0, 0, 0, 0}, []byte("vide")...))
	mdia := box("mdia", append(mdhd, hdlr...))
	trak := box("trak", mdia)
	moov := box("moov", trak)
	return append(ftypBuf.Bytes(), moov...), nil
}

// FragmentOptions are the deterministic fields encoded into a fragment.
type FragmentOptions struct {
	SequenceNumber uint32
	TrackID        uint32
	TFDT           uint64
	Payload        []byte
}

// BuildFragment creates a styp/moof/mdat fMP4 fragment.
func BuildFragment(o FragmentOptions) []byte {
	mfhdPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(mfhdPayload, o.SequenceNumber)
	mfhd := fullBox("mfhd", 0, 0, mfhdPayload)

	tfhdPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(tfhdPayload, o.TrackID)
	tfhd := fullBox("tfhd", 0, 0x020000, tfhdPayload) // default-base-is-moof

	tfdtPayload := make([]byte, 8)
	binary.BigEndian.PutUint64(tfdtPayload, o.TFDT)
	tfdt := fullBox("tfdt", 1, 0, tfdtPayload)

	traf := box("traf", append(tfhd, tfdt...))
	moof := box("moof", append(mfhd, traf...))
	stypPayload := append([]byte{'m', 's', 'd', 'h'}, 0, 0, 0, 0)
	styp := box("styp", stypPayload)
	mdat := box("mdat", o.Payload)
	return append(append(styp, moof...), mdat...)
}

var containerBoxes = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true,
	"stbl": true, "moof": true, "traf": true, "edts": true,
}

// ValidateBoxes checks box boundaries, rejects truncation, and enforces the
// 64 MiB box limit. It also asks go-mp4 to traverse the top-level structure.
func ValidateBoxes(data []byte) error {
	if len(data) < 8 {
		return media.NewError(media.CodeTruncatedBox, "repair.fmp4", "box", "fewer than 8 bytes")
	}
	if err := validateRange(data, 0, len(data)); err != nil {
		return err
	}
	_, err := mp4.ReadBoxStructure(bytes.NewReader(data), func(h *mp4.ReadHandle) (any, error) {
		return h.BoxInfo.Type.String(), nil
	})
	if err != nil {
		return media.Wrap(err, media.CodeTruncatedBox, "repair.fmp4", "box", "go-mp4 rejected box structure")
	}
	return nil
}

func validateRange(data []byte, start, end int) error {
	off := start
	for off < end {
		if end-off < 8 {
			return media.NewError(media.CodeTruncatedBox, "repair.fmp4", "box", "truncated box header")
		}
		declared := binary.BigEndian.Uint32(data[off : off+4])
		boxType := string(data[off+4 : off+8])
		header := 8
		size := int64(declared)
		if declared == 1 {
			if end-off < 16 {
				return media.NewError(media.CodeTruncatedBox, "repair.fmp4", boxType, "missing 64-bit size")
			}
			size = int64(binary.BigEndian.Uint64(data[off+8 : off+16]))
			header = 16
		} else if declared == 0 {
			size = int64(end - off)
		}
		if size > MaxFragmentBytes {
			return media.NewError(media.CodeTooLarge, "repair.fmp4", boxType, "box exceeds 64 MiB")
		}
		if size < int64(header) || off+int(size) > end {
			return media.NewError(media.CodeTruncatedBox, "repair.fmp4", boxType, "declared box exceeds buffer")
		}
		if containerBoxes[boxType] {
			if err := validateRange(data, off+header, off+int(size)); err != nil {
				return err
			}
		}
		off += int(size)
	}
	return nil
}
