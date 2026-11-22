package testdata

// inject applies the requested deterministic corruption. Each injection is
// designed to trip a specific structured error at a precise offset rather
// than producing arbitrary garbage.
func inject(spec FixtureSpec, raw []byte) []byte {
	out := append([]byte(nil), raw...)
	switch spec.Injection {
	case "":
		return out
	case "bad-magic":
		// Scramble the leading brand/UL so format dispatch rejects the file.
		for i := 0; i < 8 && i < len(out); i++ {
			out[i] ^= 0xFF
		}
	case "truncated-box":
		// MP4: inflate a box's declared 32-bit size so its boundary runs
		// past the physical EOF, then truncate the trailing bytes. The
		// walker must reject it with TRUNCATED_BOX at the box offset.
		if spec.Format == "mp4" && len(out) >= 12 {
			return corruptMP4Box(out)
		}
		cut := len(out) * 3 / 4
		if cut < 8 {
			cut = 8
		}
		return out[:cut]
	case "truncated-klv":
		// MXF: keep all headers but cut inside the final essence payload.
		cut := len(out) - 4
		if cut < 20 {
			cut = 20
		}
		return out[:cut]
	case "oversize-block":
		// Handled by the parser through stsz; here we only flip the flag so
		// callers know the spec requested it. The actual oversize table is
		// built by the dedicated test that writes its own stsz.
		return out
	}
	return out
}

// corruptMP4Box walks top-level box headers, picks the last one (mdat),
// rewrites its declared size to exceed the file, and drops the final bytes
// so the declared boundary is physically unreachable.
func corruptMP4Box(out []byte) []byte {
	pos := 0
	last := -1
	for pos+8 <= len(out) {
		size := int(uint32(out[pos])<<24 | uint32(out[pos+1])<<16 |
			uint32(out[pos+2])<<8 | uint32(out[pos+3]))
		if size < 8 || pos+size > len(out) {
			break
		}
		last = pos
		pos += size
	}
	if last < 0 {
		return out[:len(out)/2]
	}
	// Declare an impossibly large size on the last box, then cut its tail.
	for i := 0; i < 4; i++ {
		out[last+i] = 0x7f
	}
	cut := len(out) - 8
	if cut < last+8 {
		cut = last + 8
	}
	return out[:cut]
}
