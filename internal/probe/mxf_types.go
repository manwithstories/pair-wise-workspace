package probe

// mxfScan is the result of the KLV pass.
type mxfScan struct {
	primer    primer
	preface   *LocalSet
	content   *LocalSet
	packages  []*LocalSet
	tracks    []*LocalSet
	descs     []*LocalSet
	sequences map[[32]byte]*LocalSet
	clips     map[[32]byte]*LocalSet
	timecodes map[[32]byte]*LocalSet
	essence   []essenceSpan
}

type essenceSpan struct {
	trackNo uint32
	off     uint64
	length  uint64
}
