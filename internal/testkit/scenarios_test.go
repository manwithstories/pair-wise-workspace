package testkit

import (
	"testing"

	"archivecheck/internal/fault"
	"archivecheck/internal/parse"
)

func TestDemoInputsManifest(t *testing.T) {
	work := t.TempDir()
	layout, err := WriteDemoInputs(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Fragments) != 12 {
		t.Fatalf("fragments = %d", len(layout.Fragments))
	}
}

func TestFaultCorpusTruncationOffset(t *testing.T) {
	work := t.TempDir()
	fl, err := WriteFaultInputs(work)
	if err != nil {
		t.Fatal(err)
	}
	f, err := openRead(fl.Truncated)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	_, err = parse.ProbeSegment(f, info.Size(), "truncated.m4s")
	fe, ok := fault.As(err)
	if !ok {
		t.Fatalf("expected fault, got %v", err)
	}
	if fe.Offset != fl.Offset || fe.Offset != 4096 {
		t.Fatalf("offset = %d, want 4096", fe.Offset)
	}
	if fe.BoxType != "moof" {
		t.Fatalf("box = %q", fe.BoxType)
	}
}

func TestFaultCorpusBadMdatLength(t *testing.T) {
	work := t.TempDir()
	fl, err := WriteFaultInputs(work)
	if err != nil {
		t.Fatal(err)
	}
	initF, err := openRead(fl.Dir + "/init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer initF.Close()
	info, _ := initF.Stat()
	init, err := parse.ParseInitReader(initF, info.Size(), "init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	f, err := openRead(fl.BadMdatLen)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	finfo, _ := f.Stat()
	_, err = parse.ParseFragmentReader(f, finfo.Size(), "badlength.m4s", init)
	if err == nil {
		t.Fatal("expected invalid declared mdat length to fail")
	}
	if !fault.IsKind(err, fault.KindOutOfBounds) {
		t.Fatalf("err = %v", err)
	}
}
