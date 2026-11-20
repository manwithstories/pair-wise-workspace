package provenance

import (
	"path/filepath"
	"testing"
)

func TestLogAddAndFirstError(t *testing.T) {
	var l Log
	l.Add(Record{Step: "parse", Track: "subtitle", SourceFile: "a.srt",
		ByteStart: 100, ByteEnd: 200, Errors: []string{"invalid_time"}}, []byte("out1"))
	l.Add(Record{Step: "parse", Track: "subtitle", SourceFile: "a.srt",
		ByteStart: 40, ByteEnd: 90, Errors: []string{"truncated"}}, []byte("out2"))
	l.Add(Record{Step: "split", Track: "media", SourceFile: "a.mp4",
		ByteStart: 0, ByteEnd: 10}, []byte("out3"))

	off, rec := l.FirstError()
	if off != 40 || rec == nil || rec.Errors[0] != "truncated" {
		t.Fatalf("off=%d rec=%+v", off, rec)
	}
	if len(l.Records[0].SHA256) != 64 {
		t.Fatalf("sha=%q", l.Records[0].SHA256)
	}
}

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prov.json")
	var l Log
	l.Add(Record{Step: "s", Track: "t", SourceFile: "f", ByteStart: 1, ByteEnd: 2}, []byte("x"))
	if err := l.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Records) != 1 || loaded.Records[0].SHA256 != l.Records[0].SHA256 {
		t.Fatalf("loaded=%+v", loaded.Records)
	}
}

func TestFirstErrorEmpty(t *testing.T) {
	var l Log
	off, rec := l.FirstError()
	if off != -1 || rec != nil {
		t.Fatalf("off=%d rec=%v", off, rec)
	}
}
