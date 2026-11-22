package fixture

import (
	"os"
)

// Injection names accepted by the demo command.
const (
	InjectTruncatedBox = "truncated-box"
	InjectWrapDriftVFR = "wrap-drift-vfr"
)

// TruncateBox cuts tailBytes off the end of path, producing a deterministic
// truncated final box. The returned offset is the first missing byte.
func TruncateBox(path string, tailBytes int) (corruptOffset int64, err error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	corruptOffset = st.Size() - int64(tailBytes)
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err := f.Truncate(corruptOffset); err != nil {
		return 0, err
	}
	return corruptOffset, nil
}

// SupportsInjection reports whether name is a recognized injection.
func SupportsInjection(name string) bool {
	switch name {
	case InjectTruncatedBox, InjectWrapDriftVFR:
		return true
	}
	return false
}
