package mp4

import (
	"bytes"
	"fmt"

	"github.com/abema/go-mp4"
)

// BoxCount is a cross-validation view of the container produced by the
// external go-mp4 library. It is compared against the in-tree scanner so the
// two independent parsers must agree on structure and offsets.
type BoxCount struct {
	Total int
	Traks int
	Mdats int
	Stts  int
	Stss  int
	Stsz  int
	Stco  int
	Co64  int
}

// CrossValidate parses data with github.com/abema/go-mp4 and asserts that its
// box structure matches the in-tree ScanBoxes result: same box count, same
// per-type counts and identical header offsets. Any disagreement is returned
// as an error rather than silently trusted.
func CrossValidate(data []byte, roots []*Box) (BoxCount, error) {
	r := bytes.NewReader(data)
	type node struct {
		offset int64
		btype  BoxType
	}
	var libNodes []node
	handler := func(h *mp4.ReadHandle) (interface{}, error) {
		info := h.BoxInfo
		var bt BoxType
		copy(bt[:], info.Type[:])
		// The library is lenient about a final box whose declared size runs
		// past EOF; enforce the boundary explicitly so truncation is caught.
		if info.Offset+info.Size > uint64(len(data)) {
			return nil, fmt.Errorf("%w: go-mp4 box %s at %d size %d exceeds %d bytes",
				ErrTruncated, bt, info.Offset, info.Size, len(data))
		}
		libNodes = append(libNodes, node{
			offset: int64(info.Offset),
			btype:  bt,
		})
		// Expand only recognized container boxes; leaf boxes return nil so
		// the library does not attempt to interpret synthetic payloads.
		if isContainerType(bt) {
			return h.Expand()
		}
		return nil, nil
	}
	if _, err := mp4.ReadBoxStructure(r, handler); err != nil {
		// A truncated fixture is expected to fail here; the caller decides
		// whether that is the desired corruption signal.
		return BoxCount{}, fmt.Errorf("go-mp4 parse: %w", err)
	}

	var own []*Box
	var collect func(boxes []*Box)
	collect = func(boxes []*Box) {
		for _, b := range boxes {
			own = append(own, b)
			collect(b.Child)
		}
	}
	collect(roots)

	if len(libNodes) != len(own) {
		return BoxCount{}, fmt.Errorf("cross-validate: box count %d (go-mp4) != %d (in-tree)",
			len(libNodes), len(own))
	}
	counts := BoxCount{Total: len(own)}
	for i, ln := range libNodes {
		if ln.offset != own[i].Header || ln.btype != own[i].Type {
			return BoxCount{}, fmt.Errorf(
				"cross-validate: box %d mismatch go-mp4=(%d,%s) in-tree=(%d,%s)",
				i, ln.offset, ln.btype, own[i].Header, own[i].Type)
		}
		switch ln.btype {
		case BoxTrak:
			counts.Traks++
		case BoxMdat:
			counts.Mdats++
		case BoxStts:
			counts.Stts++
		case BoxStss:
			counts.Stss++
		case BoxStsz:
			counts.Stsz++
		case BoxStco:
			counts.Stco++
		case BoxCo64:
			counts.Co64++
		}
	}
	return counts, nil
}

// isContainerType mirrors Box.IsContainer for the library walk.
func isContainerType(t BoxType) bool {
	switch t {
	case BoxMoov, BoxTrak, BoxMdia, BoxMinf, BoxStbl:
		return true
	default:
		return false
	}
}
