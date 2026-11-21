package video

import (
	"testing"

	"mkvarchive/internal/model"
)

func videoTrack(uid uint64, colorPresent bool, missing []string) *model.Track {
	return &model.Track{UID: uid, Kind: model.KindVideo, HeaderOffset: int64(uid * 10),
		Video: &model.VideoMeta{
			ColorPresent:       colorPresent,
			MissingColorFields: missing,
			MatrixCoefficients: 9,
		}}
}

func TestRejectMissingColourElement(t *testing.T) {
	_, _, err := ValidateTracks([]*model.Track{videoTrack(1, false, nil)}, PolicyReject)
	ae, ok := err.(*model.ArchiveError)
	if !ok || ae.Code != "COLOR_METADATA_MISSING" || ae.Offset != 10 {
		t.Fatalf("want anchored missing-colour error, got %v", err)
	}
}

func TestRejectIncompleteColour(t *testing.T) {
	tr := videoTrack(2, true, []string{"Range", "Primaries", "TransferCharacteristics"})
	_, _, err := ValidateTracks([]*model.Track{tr}, PolicyReject)
	ae, ok := err.(*model.ArchiveError)
	if !ok || ae.Code != "COLOR_FIELDS_INCOMPLETE" {
		t.Fatalf("want incomplete error, got %v", err)
	}
}

func TestRetainCarriesWarningsAndRawFields(t *testing.T) {
	tr := videoTrack(3, true, []string{"Range"})
	reports, warnings, err := ValidateTracks([]*model.Track{tr}, PolicyRetain)
	if err != nil || len(reports) != 1 {
		t.Fatalf("retain must succeed: %v", err)
	}
	if reports[0].Matrix != 9 || len(reports[0].MissingFields) != 1 {
		t.Fatalf("raw fields not retained: %+v", reports[0])
	}
	if len(warnings) != 1 || warnings[0].Code != "COLOR_FIELDS_INCOMPLETE" {
		t.Fatalf("warning not propagated: %+v", warnings)
	}
}

func TestCompleteColourPasses(t *testing.T) {
	tr := videoTrack(4, true, nil)
	reports, warnings, err := ValidateTracks([]*model.Track{tr}, PolicyReject)
	if err != nil || len(reports) != 1 || len(warnings) != 0 {
		t.Fatalf("complete colour must pass strict: %v %+v", err, warnings)
	}
}
