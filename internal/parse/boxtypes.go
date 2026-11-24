package parse

// containerPrefix is the number of payload bytes before the first child in
// a container box. A box missing from this map is a leaf.
var containerPrefix = map[string]int{
	"moov": 0, "trak": 0, "mdia": 0, "minf": 0,
	"stbl": 0, "dinf": 0, "udta": 0, "edts": 0,
	"mvex": 0, "moof": 0, "traf": 0,
	"stsd": 8, "meta": 4, "dref": 8,
}

// containerTypes reports whether the box has children.
var containerTypes = func() map[string]bool {
	m := make(map[string]bool, len(containerPrefix))
	for k := range containerPrefix {
		m[k] = true
	}
	return m
}()

// knownLeafTypes is the set of leaf boxes the reader understands. Anything
// else is reported as unknown and preserved byte-for-byte.
var knownLeafTypes = map[string]bool{
	"ftyp": true, "mdat": true, "free": true, "skip": true,
	"mvhd": true, "tkhd": true, "mdhd": true, "hdlr": true,
	"vmhd": true, "smhd": true, "nmhd": true, "dref": true,
	"url ": true, "urn ": true, "stsd": true, "stts": true,
	"stss": true, "stsz": true, "stsc": true, "stco": true,
	"co64": true, "elst": true, "trex": true, "mfhd": true,
	"tfhd": true, "trun": true, "emsg": true, "avcC": true,
	"avc1": true, "mp4a": true, "stpp": true, "btrt": true,
	"pasp": true, "esds": true, "chpl": true, "id3 ": true,
}
