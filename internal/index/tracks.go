package index

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/abema/go-mp4"

	"mediad/internal/model"
)

type faultAdder func(*model.Fault)

// firstLeaf extracts a single leaf box under parent via the given relative
// path (parent itself is not part of the path).
func firstLeaf(r io.ReadSeeker, parent *mp4.BoxInfo, path ...mp4.BoxType) (*mp4.BoxInfo, error) {
	bis, err := mp4.ExtractBox(r, parent, mp4.BoxPath(path))
	if err != nil {
		return nil, err
	}
	if len(bis) == 0 {
		return nil, nil
	}
	return bis[0], nil
}

// readCodec reads the codec FourCC of the first sample entry inside stsd.
// SeekToPayload lands on the FullBox bytes: FullBox(4) + entry_count(4) +
// entry size(4) + FourCC(4).
func readCodec(r io.ReadSeeker, stsd *mp4.BoxInfo) (string, error) {
	if _, err := stsd.SeekToPayload(r); err != nil {
		return "", err
	}
	head := make([]byte, 16)
	if _, err := io.ReadFull(r, head); err != nil {
		return "", model.NewFault(model.FaultTruncatedAtom,
			int64(stsd.Offset+stsd.HeaderSize), "stsd", "sample entry header truncated")
	}
	return string(head[12:16]), nil
}

// parseTracks locates every trak under moov and delegates table expansion.
func parseTracks(f *os.File, idx *FileIndex, moov *AtomRec, addFault faultAdder) error {
	moovBI := &mp4.BoxInfo{
		Offset:     uint64(moov.Offset),
		Size:       uint64(moov.Size),
		HeaderSize: uint64(moov.Header),
		Type:       mp4.BoxTypeMoov(),
	}
	traks, err := mp4.ExtractBox(f, moovBI, mp4.BoxPath{mp4.StrToBoxType("trak")})
	if err != nil {
		return model.NewFault(model.FaultTruncatedAtom, moov.Offset, idx.Name,
			"cannot enumerate traks: "+err.Error())
	}
	for _, trak := range traks {
		tr, perr := parseOneTrack(f, trak, idx, addFault)
		if perr != nil {
			return perr
		}
		idx.Tracks = append(idx.Tracks, tr)
	}
	if len(idx.Tracks) == 0 {
		addFault(model.NewFault(model.FaultMissingIndex, model.OffsetUnknown,
			idx.Name, "moov contains no trak"))
	}
	return nil
}

func parseOneTrack(f *os.File, trak *mp4.BoxInfo, idx *FileIndex, addFault faultAdder) (Track, error) {
	tr := Track{Regression: -1, WrappedAt: -1}

	mdhdBI, err := firstLeaf(f, trak, mp4.StrToBoxType("mdia"), mp4.StrToBoxType("mdhd"))
	if err != nil {
		return tr, err
	}
	if mdhdBI == nil {
		return tr, model.NewFault(model.FaultMissingIndex,
			int64(trak.Offset), idx.Name, "trak missing mdhd")
	}
	mdhdBoxes, err := mp4.ExtractBoxWithPayload(f, trak,
		mp4.BoxPath{mp4.StrToBoxType("mdia"), mp4.StrToBoxType("mdhd")})
	if err != nil || len(mdhdBoxes) == 0 {
		return tr, model.NewFault(model.FaultTruncatedAtom, int64(mdhdBI.Offset),
			idx.Name, "mdhd unreadable")
	}
	mdhd := mdhdBoxes[0].Payload.(*mp4.Mdhd)
	tr.Timescale = mdhd.Timescale
	if mdhd.Timescale == 0 {
		return tr, model.NewFault(model.FaultContainerBounds, int64(mdhdBI.Offset),
			idx.Name, "mdhd timescale is zero")
	}

	hdlrBoxes, err := mp4.ExtractBoxWithPayload(f, trak,
		mp4.BoxPath{mp4.StrToBoxType("mdia"), mp4.StrToBoxType("hdlr")})
	if err != nil || len(hdlrBoxes) == 0 {
		return tr, model.NewFault(model.FaultMissingIndex, int64(trak.Offset),
			idx.Name, "trak missing hdlr")
	}
	hdlr := hdlrBoxes[0].Payload.(*mp4.Hdlr)
	tr.Handler = string(hdlr.HandlerType[:])
	tr.IsVideo = tr.Handler == "vide"
	tr.IsAudio = tr.Handler == "soun"

	tkhdBoxes, err := mp4.ExtractBoxWithPayload(f, trak,
		mp4.BoxPath{mp4.StrToBoxType("tkhd")})
	if err == nil && len(tkhdBoxes) > 0 {
		if tkhd, ok := tkhdBoxes[0].Payload.(*mp4.Tkhd); ok {
			tr.TrackID = tkhd.TrackID
		}
	}

	stblBI, err := firstLeaf(f, trak, mp4.StrToBoxType("mdia"), mp4.StrToBoxType("minf"),
		mp4.StrToBoxType("stbl"))
	if err != nil {
		return tr, err
	}
	if stblBI == nil {
		return tr, model.NewFault(model.FaultMissingIndex, int64(trak.Offset),
			idx.Name, "trak missing stbl")
	}
	stsdBI, _ := firstLeaf(f, stblBI, mp4.StrToBoxType("stsd"))
	if stsdBI == nil {
		return tr, model.NewFault(model.FaultMissingIndex, int64(stblBI.Offset),
			idx.Name, "stbl missing stsd")
	}
	codec, err := readCodec(f, stsdBI)
	if err != nil {
		return tr, err
	}
	tr.Codec = codec
	if tr.IsVideo && codec != "avc1" && codec != "avc3" {
		addFault(model.NewFault(model.FaultContainerBounds,
			int64(stsdBI.Offset), idx.Name,
			fmt.Sprintf("unsupported video codec %q (only H.264 remux supported)", codec)))
	}
	if tr.IsAudio && codec != "mp4a" {
		addFault(model.NewFault(model.FaultContainerBounds,
			int64(stsdBI.Offset), idx.Name,
			fmt.Sprintf("unsupported audio codec %q (only AAC-LC remux supported)", codec)))
	}
	if err := buildSamples(f, stblBI, idx, &tr, addFault); err != nil {
		return tr, err
	}
	return tr, nil
}

// u32 reads a big-endian uint32 at an absolute file offset.
func u32(r io.ReadSeeker, off int64) (uint32, error) {
	buf := make([]byte, 4)
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(buf), nil
}
