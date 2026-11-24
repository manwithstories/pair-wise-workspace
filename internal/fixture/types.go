package fixture

import "bytes"

// build assembles the container. mdat comes after moov so truncation removes
// samples first and can also cut the tail of moov when asked.
type builder struct {
	spec   Spec
	man    *Manifest
	buf    bytes.Buffer
	mdat   bytes.Buffer
	layout []trackLayout
}

type trackLayout struct {
	spec    TrackSpec
	offsets []uint32
	sizes   []uint32
	syncs   []uint32
	mdatLen int
}
