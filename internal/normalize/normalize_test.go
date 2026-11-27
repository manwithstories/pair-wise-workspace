package normalize

import (
	"testing"

	"aircheck/internal/fault"
	"aircheck/internal/scan"
)

func TestPlanAudioResampleAndDownmix(t *testing.T) {
	tr := scan.Track{ID: "f:1", Kind: "audio", ChannelLayout: "5.1", Channels: 6, SampleRate: 44100}
	d, err := PlanAudio(tr, DefaultAudioPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if !d.Resample || !d.Downmix || len(d.Notes) != 2 {
		t.Fatalf("bad decision: %+v", d)
	}
	args := d.Args()
	if len(args) != 4 {
		t.Fatalf("args %v", args)
	}
}

func TestPlanAudioPassthrough(t *testing.T) {
	tr := scan.Track{ID: "f:1", Kind: "audio", ChannelLayout: "stereo", Channels: 2, SampleRate: 48000}
	d, err := PlanAudio(tr, DefaultAudioPolicy())
	if err != nil || d.Resample || d.Downmix {
		t.Fatalf("want passthrough, got %+v err=%v", d, err)
	}
}

func TestPlanAudioUnknownLayoutReject(t *testing.T) {
	tr := scan.Track{ID: "f:1", Kind: "audio", ChannelLayout: "hexagonal", Channels: 6}
	_, err := PlanAudio(tr, DefaultAudioPolicy())
	if fe := fault.Of(err); fe == nil || fe.Code != fault.CodeChannelLayout || fe.Recoverable {
		t.Fatalf("want non-recoverable layout fault, got %+v", fe)
	}
	p := DefaultAudioPolicy()
	p.OnUnknownLayout = "downmix"
	d, err := PlanAudio(tr, p)
	if err != nil || !d.Downmix || len(d.Notes) == 0 {
		t.Fatalf("want downmix degrade with note, got %+v err=%v", d, err)
	}
}

func TestColorConsistent(t *testing.T) {
	tr := scan.Track{ID: "f:0", Kind: "video", ColorSpace: "bt709", ColorPrim: "bt709", ColorTransfer: "bt709"}
	d, err := CheckColor(tr, DefaultColorPolicy())
	if err != nil || d.Family != "bt709" || d.Retag {
		t.Fatalf("got %+v err=%v", d, err)
	}
}

func TestColorUnspecified(t *testing.T) {
	tr := scan.Track{ID: "f:0", Kind: "video"}
	d, err := CheckColor(tr, DefaultColorPolicy())
	if err != nil || !d.Retag || len(d.Notes) == 0 {
		t.Fatalf("got %+v err=%v", d, err)
	}
}

func TestColorConflictRejectAndDegrade(t *testing.T) {
	tr := scan.Track{ID: "f:0", Kind: "video", ColorSpace: "bt709", ColorPrim: "bt709", ColorTransfer: "smpte170m"}
	_, err := CheckColor(tr, DefaultColorPolicy())
	if fe := fault.Of(err); fe == nil || fe.Code != fault.CodeColorConflict {
		t.Fatalf("want conflict fault, got %+v", fe)
	}
	p := DefaultColorPolicy()
	p.OnConflict = "tag"
	d, err := CheckColor(tr, p)
	if err != nil || !d.Retag || len(d.Notes) == 0 {
		t.Fatalf("want degrade with note, got %+v err=%v", d, err)
	}
}

func TestColorFamilyMismatchRetag(t *testing.T) {
	tr := scan.Track{ID: "f:0", Kind: "video", ColorSpace: "smpte170m", ColorPrim: "smpte170m", ColorTransfer: "smpte170m"}
	d, err := CheckColor(tr, DefaultColorPolicy())
	if err != nil || d.Family != "bt601" || !d.Retag {
		t.Fatalf("got %+v err=%v", d, err)
	}
}
