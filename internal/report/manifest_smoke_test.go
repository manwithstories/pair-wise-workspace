package report

import "testing"

func TestSidecarRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{SourceSHA256: "abc", TargetBps: 1}
	if err := Write(dir, m); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySidecar(dir, loaded); err != nil {
		t.Fatal(err)
	}
	loaded.TargetBps = 2
	if err := VerifySidecar(dir, loaded); err == nil {
		t.Fatal("expected digest mismatch")
	}
}
