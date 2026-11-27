package sample

// Fault injection. All injections act on media items only (RTCP reports
// are never dropped or reordered), and the ground-truth anomaly index
// is computed mechanically as the first position where the final
// stream deviates from the ideal one.

// InjectFaults applies the named fault profile to the ideal stream and
// returns the final stream plus the ideal index at which the first
// anomaly becomes visible in the delivered stream. total is the
// delivered stream length used to keep late packets in range.
func InjectFaults(ideal []Item, seed int64, profile string, total int64) ([]Item, int64) {
	items := append([]Item(nil), ideal...)
	if profile == "none" {
		return items, -1
	}
	r := prng(seed, 0xfa07)

	// 1. A same-track adjacent swap early in the stream: the first
	// anomaly, immediately detectable at its arrival index.
	t1 := trackItems(items, 0)
	swapAt := t1[50]
	items[swapAt], items[t1[51]] = items[t1[51]], items[swapAt]

	// 2. Random same-track swaps (~0.3% of media items), after id 1000.
	media := mediaIndices(items, 1000)
	for i := 0; i+1 < len(media); i++ {
		if r.next()%1024 < 3 {
			a, b := media[i], media[i+1]
			if items[a].Track == items[b].Track {
				items[a], items[b] = items[b], items[a]
			}
		}
	}

	// 3. Isolated losses (~0.4%) plus one unrecoverable burst of 20
	// consecutive packets of the video track 3.
	drop := make(map[int]bool)
	for _, idx := range mediaIndices(items, 1000) {
		if r.next()%1024 < 4 {
			drop[idx] = true
		}
	}
	t3 := trackItems(items, 3)
	burstStart := t3[len(t3)*3/10]
	burst := make(map[int]bool)
	for _, idx := range t3[len(t3)*3/10 : len(t3)*3/10+20] {
		burst[idx] = true
	}
	_ = burstStart
	kept := items[:0]
	for i, it := range items {
		if drop[i] || burst[i] {
			continue
		}
		kept = append(kept, it)
	}
	items = kept

	// 4. Duplicates (~0.2%): insert a copy right after the original.
	var dupd []Item
	for i, it := range items {
		dupd = append(dupd, it)
		if !it.IsRTCP && it.ID > 1000 && r.next()%1024 < 2 {
			dupd = append(dupd, it)
		}
		_ = i
	}
	items = dupd

	// 5. Late packets: 20 re-inserted 40 positions later (inside the
	// rewind window) and 10 re-inserted 20000 positions later (outside).
	items = laten(items, r, 20, 40, total-100)
	items = laten(items, r, 10, 20000, total/2)

	// Ground truth: first position where the delivered stream deviates
	// from the ideal order.
	truth := int64(-1)
	for i := 0; i < len(items) && i < len(ideal); i++ {
		if items[i].ID != ideal[i].ID {
			truth = int64(i)
			break
		}
	}
	return items, truth
}

// trackItems returns the stream indices of media items of one track.
func trackItems(items []Item, track int) []int {
	var out []int
	for i, it := range items {
		if !it.IsRTCP && it.Track == track {
			out = append(out, i)
		}
	}
	return out
}

// mediaIndices returns the stream indices of media items with ID >= minID.
func mediaIndices(items []Item, minID int64) []int {
	var out []int
	for i, it := range items {
		if !it.IsRTCP && it.ID >= minID {
			out = append(out, i)
		}
	}
	return out
}

// laten moves n roughly evenly spaced media items (with ID < maxID)
// delay positions later.
func laten(items []Item, r rng, n int, delay int, maxID int64) []Item {
	var cand []int
	for i, it := range items {
		if !it.IsRTCP && it.ID >= 2000 && it.ID < maxID {
			cand = append(cand, i)
		}
	}
	if len(cand) == 0 || n <= 0 {
		return items
	}
	stride := len(cand) / n
	if stride < 1 {
		stride = 1
	}
	var picked []int
	for k := 0; k < n; k++ {
		j := k*stride + int(r.next()%uint64(stride))
		if j >= len(cand) {
			j = len(cand) - 1
		}
		picked = append(picked, cand[j])
	}
	// Move from the last picked position backwards so earlier indices
	// stay valid.
	for k := len(picked) - 1; k >= 0; k-- {
		i := picked[k]
		it := items[i]
		items = append(items[:i], items[i+1:]...)
		j := i + delay
		if j > len(items) {
			j = len(items)
		}
		items = append(items[:j], append([]Item{it}, items[j:]...)...)
	}
	return items
}

// Generate builds the ideal stream with margin, applies the fault
// profile and trims the result to exactly packets items, returning the
// final stream plus the ground-truth anomaly index (-1 when the
// profile injects no faults).
func Generate(seed int64, packets int, profile string) ([]Item, int64) {
	ideal := BuildIdeal(packets * 2)
	items, truth := InjectFaults(ideal, seed, profile, int64(packets))
	if len(items) > packets {
		items = items[:packets]
	}
	return items, truth
}
