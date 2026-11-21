package mp4

// UnknownBoxes returns the types of boxes the parser preserved verbatim
// because go-mp4 has no typed definition for them.  Callers use it to warn
// operators that vendor extensions were carried through rather than dropped.
func (f *File) UnknownBoxes() []string {
	out := make([]string, len(f.Unknown))
	copy(out, f.Unknown)
	return out
}
