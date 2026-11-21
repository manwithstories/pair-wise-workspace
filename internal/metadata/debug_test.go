package metadata

import (
	"fmt"
	"os"
	"testing"
)

func TestDebugMXFBytes(t *testing.T) {
	dir := t.TempDir()
	p, _ := WriteDemoMXF(dir, false)
	b, _ := readAll(p)
	fmt.Printf("% x\n", b)
}

func readAll(p string) ([]byte, error) { return os.ReadFile(p) }
