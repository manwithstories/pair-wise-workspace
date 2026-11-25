package delivery

import (
	"testing"

	"github.com/abema/go-mp4"
)

func TestProbeMarshalBoxes(t *testing.T) {
	cases := []struct {
		name string
		box  mp4.IImmutableBox
	}{
		{"ftyp", &mp4.Ftyp{MajorBrand: [4]byte{'i', 's', 'o', 'm'}}},
		{"mvhd", &mp4.Mvhd{Timescale: 90000}},
		{"tkhd", &mp4.Tkhd{TrackID: 1}},
		{"mdhd", &mp4.Mdhd{Timescale: 90000}},
		{"hdlr", &mp4.Hdlr{HandlerType: [4]byte{'v', 'i', 'd', 'e'}, Name: "Video"}},
		{"vmhd", &mp4.Vmhd{}},
		{"dref", &mp4.Dref{EntryCount: 1}},
		{"url", &mp4.Url{FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 1}}}},
		{"stsd", &mp4.Stsd{EntryCount: 1}},
		{"smhd", &mp4.Smhd{}},
		{"stts", &mp4.Stts{}},
		{"stsc", &mp4.Stsc{}},
		{"stsz", &mp4.Stsz{}},
		{"stco", &mp4.Stco{}},
		{"trex", &mp4.Trex{TrackID: 1}},
		{"mfhd", &mp4.Mfhd{SequenceNumber: 1}},
		{"tfhd", &mp4.Tfhd{TrackID: 1}},
		{"tfdt", &mp4.Tfdt{FullBox: mp4.FullBox{Version: 1}}},
	}
	buf := &fileBuffer{}
	for _, c := range cases {
		if _, err := mp4.Marshal(buf, c.box, mp4.Context{}); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// Sample entries marshal only via AnyType with SetType.
	entry := &mp4.VisualSampleEntry{SampleEntry: mp4.SampleEntry{DataReferenceIndex: 1}, Width: 1920, Height: 1080}
	entry.SetType(mp4.BoxTypeAvc1())
	if _, err := mp4.Marshal(buf, entry, mp4.Context{}); err != nil {
		t.Errorf("avc1: %v", err)
	}
	audio := &mp4.AudioSampleEntry{SampleEntry: mp4.SampleEntry{DataReferenceIndex: 1}, ChannelCount: 2, SampleSize: 16, SampleRate: 48000 << 16}
	audio.SetType(mp4.BoxTypeMp4a())
	if _, err := mp4.Marshal(buf, audio, mp4.Context{}); err != nil {
		t.Errorf("mp4a: %v", err)
	}
	trun := &mp4.Trun{SampleCount: 0}
	if _, err := mp4.Marshal(buf, trun, mp4.Context{}); err != nil {
		t.Logf("trun with zero entries: %v (expected)", err)
	}
	avcc := &mp4.AVCDecoderConfiguration{ConfigurationVersion: 1, LengthSizeMinusOne: 3}
	avcc.SequenceParameterSets = []mp4.AVCParameterSet{{Length: 4, NALUnit: []byte{0x67, 0x64, 0, 0x28}}}
	avcc.NumOfSequenceParameterSets = 1
	avcc.PictureParameterSets = []mp4.AVCParameterSet{{Length: 4, NALUnit: []byte{0x68, 0, 0, 0}}}
	avcc.NumOfPictureParameterSets = 1
	if _, err := mp4.Marshal(buf, avcc, mp4.Context{}); err != nil {
		t.Errorf("avcC: %v", err)
	}
	colr := &mp4.Colr{ColourType: [4]byte{'n', 'c', 'l', 'x'}, ColourPrimaries: 1, TransferCharacteristics: 1, MatrixCoefficients: 1}
	if _, err := mp4.Marshal(buf, colr, mp4.Context{}); err != nil {
		t.Errorf("colr: %v", err)
	}
	// Empty avcC sequence sets, as produced when SPS is a placeholder.
	empty := &mp4.AVCDecoderConfiguration{ConfigurationVersion: 1, LengthSizeMinusOne: 3}
	if _, err := mp4.Marshal(buf, empty, mp4.Context{}); err != nil {
		t.Errorf("avcC empty: %v", err)
	}
}
