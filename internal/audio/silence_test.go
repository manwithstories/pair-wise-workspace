package audio_test

import (
	"testing"

	"mediaproc/internal/audio"
	"mediaproc/internal/demux"
	"mediaproc/internal/ebml"
	"mediaproc/internal/model"
	"mediaproc/internal/sample"
)

func opusBlock(track uint64, ms int64, packet []byte) *demux.Block {
	return &demux.Block{
		TrackNumber: track,
		TimeNS:      model.Nanoseconds(ms) * model.Millisecond,
		Track: &demux.Track{
			Number:  track,
			Type:    ebml.TrackTypeAudio,
			CodecID: "A_OPUS",
		},
		Frames: [][]byte{packet},
		Data:   packet,
	}
}

func TestDetectsTwoWindowsAndHysteresisExit(t *testing.T) {
	d := audio.NewDetector()
	windows := d.AnalyzeBlock(opusBlock(2, 200, sample.SilencePacket()))
	if len(windows) != 2 {
		t.Fatalf("windows=%d want 2", len(windows))
	}
	for i, w := range windows {
		if !w.Silent || w.RMS >= d.SilenceEnter {
			t.Fatalf("window %d not silent: %+v", i, w)
		}
	}
	if d.SilentWindows != 2 {
		t.Fatalf("silent windows=%d want 2", d.SilentWindows)
	}

	voice := d.AnalyzeBlock(opusBlock(2, 240, sample.StrongVoicePacket()))
	if len(voice) != 1 {
		t.Fatalf("voice windows=%d want 1", len(voice))
	}
	if voice[0].Silent {
		t.Fatalf("strong voice remained silent under hysteresis: rms=%f", voice[0].RMS)
	}
}

func TestUnsupportedAudioRetainsBlockAndReportsTrackError(t *testing.T) {
	d := audio.NewDetector()
	original := &demux.Block{
		TrackNumber: 3,
		Offset:      1234,
		TimeNS:      model.Second,
		Track: &demux.Track{
			Number:  3,
			Type:    ebml.TrackTypeAudio,
			CodecID: "A_MPEG/L3",
		},
		Frames: [][]byte{{0xff, 0xfb, 0x90}},
		Data:   []byte{0xff, 0xfb, 0x90},
	}
	if got := d.AnalyzeBlock(original); got != nil {
		t.Fatalf("unsupported codec produced windows: %v", got)
	}
	if len(d.TrackErrors) != 1 || d.TrackErrors[0].TrackNumber != 3 ||
		d.TrackErrors[0].Offset != 1234 {
		t.Fatalf("track errors=%+v", d.TrackErrors)
	}
	if original.Data[0] != 0xff || len(original.Frames[0]) != 3 {
		t.Fatal("original unsupported block was not retained")
	}
}
