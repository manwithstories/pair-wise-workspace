package adapter

import (
	"bytes"
	"io"

	"github.com/abema/go-mp4"

	"avpipe/internal/media"
)

// Box type four-char codes used inside the side-car index.
var (
	boxFTYP = mp4.StrToBoxType("ftyp")
	boxAVPI = mp4.StrToBoxType("avpi")
)

// MP4MaxIndexBytes bounds the side-car file (16 MiB).
const MP4MaxIndexBytes = 16 << 20

// WriteMP4Index serializes the index rows into a minimal valid MP4 file:
// an ftyp box followed by one custom "avpi" box holding the binary rows.
func WriteMP4Index(entries []IndexEntry) ([]byte, error) {
	var buf bytes.Buffer

	// ftyp: major_brand "isom", minor_version 512, two compatible brands.
	ftypPayload := make([]byte, 0, 16)
	ftypPayload = append(ftypPayload, []byte("isom")...)
	ver := make([]byte, 4)
	putU32(ver, 512)
	ftypPayload = append(ftypPayload, ver...)
	ftypPayload = append(ftypPayload, []byte("isom")...)
	ftypPayload = append(ftypPayload, []byte("avpi")...)
	if err := writeRawBox(&buf, boxFTYP, ftypPayload); err != nil {
		return nil, err
	}
	if err := writeRawBox(&buf, boxAVPI, encodeIndexPayload(entries)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeRawBox emits an 8-byte small MP4 header followed by the payload.
func writeRawBox(buf *bytes.Buffer, typ mp4.BoxType, payload []byte) error {
	hdr := mp4.EncodeBoxInfo(&mp4.BoxInfo{
		Type:       typ,
		Size:       uint64(mp4.SmallHeaderSize + len(payload)),
		HeaderSize: mp4.SmallHeaderSize,
	})
	if _, err := buf.Write(hdr); err != nil {
		return err
	}
	_, err := buf.Write(payload)
	return err
}

// encodeIndexPayload prefixes rows with a version byte and uint32 count.
func encodeIndexPayload(entries []IndexEntry) []byte {
	out := make([]byte, 5+len(entries)*IndexEntrySize)
	out[0] = ContainerVersion1
	putU32(out[1:5], uint32(len(entries)))
	for i, e := range entries {
		copy(out[5+i*IndexEntrySize:], marshalIndexEntry(e))
	}
	return out
}

// ReadMP4Index parses the avpi box back out of a side-car MP4. A missing or
// corrupt avpi box is a locatable failure; callers decide on the fallback.
func ReadMP4Index(r io.ReadSeeker, size int64) ([]IndexEntry, error) {
	if size <= 0 || size > MP4MaxIndexBytes {
		return nil, media.NewError("mp4", media.KindLimitExceeded, 0,
			media.ActionFail, "mp4 size %d out of allowed range", size)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var payload []byte
	var off int64
	for {
		bi, err := mp4.ReadBoxInfo(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, media.NewError("mp4", "box_read", off, media.ActionFail, "%v", err)
		}
		if bi.Type == boxAVPI {
			payload = make([]byte, bi.Size-bi.HeaderSize)
			if _, err := io.ReadFull(r, payload); err != nil {
				return nil, media.NewError("mp4", media.KindTruncated, off,
					media.ActionFail, "avpi payload truncated: %v", err)
			}
			break
		}
		if _, err := r.Seek(int64(bi.Offset+bi.Size), io.SeekStart); err != nil {
			return nil, err
		}
		off = int64(bi.Offset + bi.Size)
	}
	if payload == nil {
		return nil, media.NewError("mp4", media.KindMissingIndex, 0,
			media.ActionFallbackIndex, "no avpi index box found")
	}
	return decodeIndexPayload(payload)
}

func decodeIndexPayload(payload []byte) ([]IndexEntry, error) {
	if len(payload) < 5 {
		return nil, media.NewError("mp4", media.KindTruncated, 0,
			media.ActionFail, "avpi payload shorter than header")
	}
	if payload[0] != ContainerVersion1 {
		return nil, media.NewError("mp4", media.KindUnknownVersion, 0,
			media.ActionFail, "index version %d unsupported", payload[0])
	}
	count := u32(payload[1:5])
	need, overflow := media.MulU64(uint64(count), IndexEntrySize)
	if overflow || uint64(len(payload)-5) < need {
		return nil, media.NewError("mp4", media.KindOutOfBounds, 0,
			media.ActionFail, "index row count %d exceeds payload", count)
	}
	entries := make([]IndexEntry, count)
	for i := range entries {
		entries[i] = parseIndexEntry(payload[5+i*IndexEntrySize:])
	}
	return entries, nil
}
