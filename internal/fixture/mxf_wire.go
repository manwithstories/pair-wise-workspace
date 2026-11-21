package fixture

import "mediad/internal/probe"

// localSet serialises a set from ordered (tag, value) pairs.
type item struct {
	tag uint16
	val []byte
}

func klv(key probe.UL, value []byte) []byte {
	out := make([]byte, 0, 16+9+len(value))
	out = append(out, key[:]...)
	out = append(out, probe.EncodeBER(uint64(len(value)))...)
	return append(out, value...)
}

func localSet(key probe.UL, items []item) []byte {
	var body []byte
	for _, it := range items {
		body = append(body, byte(it.tag>>8), byte(it.tag))
		body = append(body, probe.EncodeBER(uint64(len(it.val)))...)
		body = append(body, it.val...)
	}
	return klv(key, body)
}

func mustTag(name string) uint16 {
	t, ok := probe.TagFor(name)
	if !ok {
		panic("unknown MXF property " + name)
	}
	return t
}

func descUL(video bool) probe.UL {
	if video {
		return probe.MustUL("060e2b34.02530101.0d010101.01012800")
	}
	return probe.MustUL("060e2b34.02530101.0d010101.01012900")
}

func boolU32(v bool) uint32 {
	if v {
		return 1
	}
	return 0
}
