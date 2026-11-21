package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"archivecheck.local/archivecheck/internal/pipeline"
)

// repManifest adapts a report for manifest writing.
type repManifest struct{ rep *pipeline.Report }

// writeManifestFile writes the recovery manifest next to the output. A failure
// to write it is reported but does not discard the verified output.
func writeManifestFile(m *repManifest, path string, out, errOut io.Writer) {
	if m == nil || m.rep == nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		fmt.Fprintf(errOut, "manifest_error=%v\n", err)
		return
	}
	tmp := f.Name()
	if err := pipeline.WriteManifest(f, m.rep); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		fmt.Fprintf(errOut, "manifest_error=%v\n", err)
		return
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		fmt.Fprintf(errOut, "manifest_error=%v\n", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		fmt.Fprintf(errOut, "manifest_error=%v\n", err)
		return
	}
	fmt.Fprintf(out, "manifest=%s\n", path)
}
