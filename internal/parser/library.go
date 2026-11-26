package parser

import gomp4 "github.com/abema/go-mp4"

// ParserLibrary identifies the pinned underlying ISO-BMFF library.
const ParserLibrary = "github.com/abema/go-mp4 v1.7.3"

// LibrarySupportsBox reports whether go-mp4 has a registered payload decoder
// for typ. The scanner still uses its bounded header/table parser so malformed
// fixtures can be diagnosed at exact byte offsets without aborting.
func LibrarySupportsBox(typ string) bool {
	return gomp4.StrToBoxType(typ).IsSupported(gomp4.Context{})
}
