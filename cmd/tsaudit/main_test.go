package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func cliPacket(pid uint16, cc uint8) []byte {
	p := bytes.Repeat([]byte{0xff}, 188)
	p[0] = 0x47
	p[1], p[2], p[3] = byte(pid>>8), byte(pid), 0x10|cc
	return p
}

func TestExitCodes(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"unknown"}, &out, &errOut); code != exitInvalidArgs {
		t.Fatalf("unknown command exit = %d", code)
	}
	if code := run([]string{"scan", "--format", "xml"}, &out, &errOut); code != exitInvalidArgs {
		t.Fatalf("invalid flag exit = %d", code)
	}
	if code := run([]string{"scan", "--input", filepath.Join(t.TempDir(), "missing.ts")}, &out, &errOut); code != exitInput {
		t.Fatalf("missing input exit = %d", code)
	}
	if code := run([]string{"scan", "--input", "x", "--fail-on", "bogus"}, &out, &errOut); code != exitInvalidArgs {
		t.Fatalf("invalid fail-on exit = %d", code)
	}
}

func TestFailOnExitCode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jump.ts")
	stream := append(cliPacket(0x100, 0), cliPacket(0x100, 2)...)
	if err := writeFile(path, stream); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run([]string{"scan", "--input", path, "--format", "json", "--fail-on", "cc_jumps"}, &out, &errOut)
	if code != exitFailOn {
		t.Fatalf("fail-on exit = %d, stderr=%s", code, errOut.String())
	}
	if !bytes.Contains(out.Bytes(), []byte(`"cc_jumps": 1`)) {
		t.Fatalf("JSON output missing cc_jumps: %s", out.String())
	}
}

func TestUnsupported192ExitCode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.ts")
	var stream []byte
	for i := 0; i < 5; i++ {
		stream = append(stream, 1, 2, 3, 4)
		stream = append(stream, cliPacket(0x20, uint8(i))...)
	}
	if err := writeFile(path, stream); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"scan", "--input", path}, &out, &errOut); code != exitInput {
		t.Fatalf("192-byte input exit = %d", code)
	}
}

func writeFile(path string, data []byte) error { return os.WriteFile(path, data, 0o644) }
