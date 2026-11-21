package track

import (
	"bytes"
	"io"

	"mkvarchive/internal/ebml"
	"mkvarchive/internal/model"
)

// RenumberTracks rewrites TrackNumber elements inside a verbatim Tracks
// payload according to the source-UID -> archive-id mapping. Every other
// byte (codec, audio, colour metadata) is copied unchanged, so archived
// metadata stays semantically identical apart from the track numbering.
func RenumberTracks(tracksData []byte, mappings []Mapping) ([]byte, error) {
	byUID := map[uint64]uint64{}
	for _, m := range mappings {
		byUID[m.SourceUID] = m.ArchiveID
	}
	var out bytes.Buffer
	r := ebml.NewReader(bytes.NewReader(tracksData))
	for {
		e, err := r.Next()
		if err == io.EOF {
			return out.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
		body, err := r.ReadData(e)
		if err != nil {
			return nil, err
		}
		if e.ID != model.IDTrackEntry {
			out.Write(ebml.EncodeElement(e.ID, body))
			continue
		}
		rewritten, err := renumberEntry(body, e.DataOff, byUID)
		if err != nil {
			return nil, err
		}
		out.Write(ebml.EncodeElement(model.IDTrackEntry, rewritten))
	}
}

func renumberEntry(body []byte, baseOff int64, byUID map[uint64]uint64) ([]byte, error) {
	var out bytes.Buffer
	var uid uint64
	// First pass: capture the entry's TrackUID.
	pr := ebml.NewReaderAtOffset(bytes.NewReader(body), baseOff)
	for {
		e, err := pr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		data, err := pr.ReadData(e)
		if err != nil {
			return nil, err
		}
		if e.ID == model.IDTrackUID {
			uid = ebml.DecodeUnsigned(data)
		}
	}
	newNumber, ok := byUID[uid]
	if !ok {
		return nil, model.NewError("TRACK_MAPPING_FAILED",
			"no archive mapping for TrackEntry", baseOff)
	}
	// Second pass: copy every child verbatim except TrackNumber.
	wr := ebml.NewReaderAtOffset(bytes.NewReader(body), baseOff)
	for {
		e, err := wr.Next()
		if err == io.EOF {
			return out.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
		data, err := wr.ReadData(e)
		if err != nil {
			return nil, err
		}
		if e.ID == model.IDTrackNumber {
			out.Write(ebml.EncodeElement(model.IDTrackNumber, ebml.EncodeUnsigned(newNumber)))
			continue
		}
		out.Write(ebml.EncodeElement(e.ID, data))
	}
}
