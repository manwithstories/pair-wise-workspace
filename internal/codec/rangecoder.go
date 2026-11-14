package codec

import "fmt"

// This file implements a byte-oriented adaptive range coder
// (LZMA-style carry handling) with an order-0 Fenwick-tree model.

const (
	rcTop      = 1 << 24
	rcModelMax = 1 << 15
)

// freqModel is an adaptive order-0 frequency table over 256 symbols.
type freqModel struct {
	tree  [256]uint32 // Fenwick tree, index i covers symbol i
	total uint32
}

func newFreqModel() *freqModel {
	m := &freqModel{}
	for i := 0; i < 256; i++ {
		m.add(i, 1)
	}
	return m
}

func (m *freqModel) add(sym int, delta int32) {
	for i := sym + 1; i <= 256; i += i & -i {
		m.tree[i-1] = uint32(int32(m.tree[i-1]) + delta)
	}
	m.total = uint32(int32(m.total) + delta)
}

// prefix returns the cumulative frequency of symbols [0, sym).
func (m *freqModel) prefix(sym int) uint32 {
	var s uint32
	for i := sym; i > 0; i -= i & -i {
		s += m.tree[i-1]
	}
	return s
}

func (m *freqModel) freq(sym int) uint32 { return m.prefix(sym+1) - m.prefix(sym) }

func (m *freqModel) update(sym int) {
	m.add(sym, 1)
	if m.total >= rcModelMax {
		var fs [256]uint32
		for i := range fs {
			fs[i] = m.freq(i)/2 + 1
		}
		*m = freqModel{}
		for i, f := range fs {
			m.add(i, int32(f))
		}
	}
}

// find returns the symbol whose cumulative interval contains target.
func (m *freqModel) find(target uint32) int {
	idx := 0
	var acc uint32
	for bit := 256; bit > 0; bit >>= 1 {
		next := idx + bit
		if next <= 256 && acc+m.tree[next-1] <= target {
			acc += m.tree[next-1]
			idx = next
		}
	}
	return idx
}

// rcEncoder range-encodes symbols into a byte slice.
type rcEncoder struct {
	out       []byte
	low       uint64
	rng       uint32
	cache     byte
	cacheSize uint64
}

func newRcEncoder() *rcEncoder { return &rcEncoder{rng: 0xFFFFFFFF, cacheSize: 1} }

func (e *rcEncoder) shiftLow() {
	if uint32(e.low) < 0xFF000000 || uint32(e.low>>32) != 0 {
		temp := e.cache
		for {
			e.out = append(e.out, temp+byte(e.low>>32))
			temp = 0xFF
			e.cacheSize--
			if e.cacheSize == 0 {
				break
			}
		}
		e.cache = byte(e.low >> 24)
	}
	e.cacheSize++
	e.low = uint64(uint32(e.low) << 8)
}

func (e *rcEncoder) encode(m *freqModel, sym int) {
	e.rng /= m.total
	e.low += uint64(m.prefix(sym)) * uint64(e.rng)
	e.rng *= m.freq(sym)
	for e.rng < rcTop {
		e.shiftLow()
		e.rng <<= 8
	}
	m.update(sym)
}

func (e *rcEncoder) flush() {
	for i := 0; i < 5; i++ {
		e.shiftLow()
	}
}

// rcDecoder reverses rcEncoder.
type rcDecoder struct {
	buf  []byte
	pos  int
	code uint32
	rng  uint32
}

func newRcDecoder(buf []byte) (*rcDecoder, error) {
	if len(buf) < 5 {
		return nil, fmt.Errorf("range-coded payload too short: %d bytes", len(buf))
	}
	d := &rcDecoder{buf: buf, pos: 1, rng: 0xFFFFFFFF}
	for i := 0; i < 4; i++ {
		d.code = d.code<<8 | uint32(buf[d.pos])
		d.pos++
	}
	return d, nil
}

func (d *rcDecoder) decode(m *freqModel) (int, error) {
	d.rng /= m.total
	target := d.code / d.rng
	if target >= m.total {
		return 0, fmt.Errorf("corrupt range-coded stream")
	}
	sym := m.find(target)
	d.code -= m.prefix(sym) * d.rng
	d.rng *= m.freq(sym)
	for d.rng < rcTop {
		if d.pos >= len(d.buf) {
			return 0, fmt.Errorf("truncated range-coded stream")
		}
		d.code = d.code<<8 | uint32(d.buf[d.pos])
		d.pos++
		d.rng <<= 8
	}
	m.update(sym)
	return sym, nil
}

// rangeCompress encodes src into a self-contained byte slice.
func rangeCompress(src []byte) []byte {
	e := newRcEncoder()
	m := newFreqModel()
	for _, b := range src {
		e.encode(m, int(b))
	}
	e.flush()
	return e.out
}

// rangeDecompress decodes rawLen bytes from a range-coded slice.
func rangeDecompress(src []byte, rawLen int) ([]byte, error) {
	d, err := newRcDecoder(src)
	if err != nil {
		return nil, err
	}
	m := newFreqModel()
	out := make([]byte, rawLen)
	for i := range out {
		sym, err := d.decode(m)
		if err != nil {
			return nil, err
		}
		out[i] = byte(sym)
	}
	return out, nil
}
