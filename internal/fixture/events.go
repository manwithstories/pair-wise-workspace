package fixture

// genSubtitles places cfg.Subtitles cues evenly over the span.
func genSubtitles(cfg Config, _ NumDen, spanUS int64) []Event {
	if cfg.Subtitles == 0 {
		return nil
	}
	var out []Event
	step := spanUS / int64(cfg.Subtitles)
	for i := 0; i < cfg.Subtitles; i++ {
		trueUS := step * int64(i)
		out = append(out, Event{
			Track:    3,
			Kind:     EventSubtitle,
			RawTS:    trueUS / 1000, // millisecond timebase
			TrueUS:   trueUS,
			Duration: step / 2,
			Payload:  cuePayload(cfg.Seed, 3, uint32(i)),
		})
	}
	return out
}

// genChapters emits chapter markers on track 4.
func genChapters(cfg Config, _ NumDen, spanUS int64) []Event {
	if cfg.Chapters == 0 {
		return nil
	}
	var out []Event
	step := spanUS / int64(cfg.Chapters)
	for i := 0; i < cfg.Chapters; i++ {
		trueUS := step * int64(i)
		out = append(out, Event{
			Track:    4,
			Kind:     EventChapter,
			RawTS:    trueUS / 1000,
			TrueUS:   trueUS,
			Duration: step,
			Payload:  cuePayload(cfg.Seed, 4, uint32(i)),
		})
	}
	return out
}

// genMeta cycles ID3 / emsg / SEI across the span on the metadata track.
func genMeta(cfg Config, _ NumDen, spanUS int64) []Event {
	if cfg.MetaEvents == 0 {
		return nil
	}
	kinds := []EventKind{EventID3, EventEmsg, EventSEI}
	var out []Event
	step := spanUS / int64(cfg.MetaEvents)
	for i := 0; i < cfg.MetaEvents; i++ {
		trueUS := step * int64(i)
		out = append(out, Event{
			Track:    5,
			Kind:     kinds[i%len(kinds)],
			RawTS:    trueUS / 1000,
			TrueUS:   trueUS,
			Duration: 0,
			Payload:  cuePayload(cfg.Seed, 5, uint32(i)),
		})
	}
	return out
}

func cuePayload(seed int64, track uint32, idx uint32) []byte {
	b := make([]byte, 48)
	deterministicPayload(b, seed, track, idx)
	return b
}
