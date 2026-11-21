package track

import (
	"testing"

	"mkvarchive/internal/ebml"
	"mkvarchive/internal/model"
)

func TestRenumberTracksOnlyChangesTrackNumber(t *testing.T) {
	video := ebml.EncodeElement(model.IDTrackEntry, cat(
		ebml.EncodeElement(model.IDTrackUID, ebml.EncodeUnsigned(1)),
		ebml.EncodeElement(model.IDTrackNumber, ebml.EncodeUnsigned(1)),
		ebml.EncodeElement(model.IDTrackType, ebml.EncodeUnsigned(1)),
		ebml.EncodeElement(model.IDCodecID, []byte("V_VP9")),
	))
	audio := ebml.EncodeElement(model.IDTrackEntry, cat(
		ebml.EncodeElement(model.IDTrackUID, ebml.EncodeUnsigned(2)),
		ebml.EncodeElement(model.IDTrackNumber, ebml.EncodeUnsigned(2)),
		ebml.EncodeElement(model.IDTrackType, ebml.EncodeUnsigned(2)),
		ebml.EncodeElement(model.IDCodecID, []byte("A_OPUS")),
	))
	tracksData := append(video, audio...)
	mappings := []Mapping{
		{SourceUID: 1, SourceNumber: 1, ArchiveID: 10, Kind: model.KindVideo},
		{SourceUID: 2, SourceNumber: 2, ArchiveID: 20, Kind: model.KindAudio},
	}
	out, err := RenumberTracks(tracksData, mappings)
	if err != nil {
		t.Fatalf("renumber: %v", err)
	}
	got, err := ParseTracks(out)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	numbers := map[uint64]uint64{}
	for _, tr := range got {
		numbers[tr.UID] = tr.Number
		if tr.CodecID == "" {
			t.Fatal("non-TrackNumber metadata must be retained verbatim")
		}
	}
	if numbers[1] != 10 || numbers[2] != 20 {
		t.Fatalf("renumber result wrong: %+v", numbers)
	}
}
