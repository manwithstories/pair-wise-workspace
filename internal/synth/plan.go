package synth

type lossWindow struct {
	start int
	end   int
	drops map[uint16]int
}

func makePlan(cfg Config) ([]int, [][]int, int) {
	if cfg.Bumps == 0 {
		return nil, nil, 0
	}
	epochs := cfg.Bumps + max(0, cfg.Bumps-2)
	positions := make([]int, epochs)
	plan := make([][]int, epochs)
	for i := range positions {
		positions[i] = cfg.Packets * (i + 1) / (epochs + 1)
	}
	if cfg.Programs == 3 && epochs == 8 {
		plan = [][]int{{0}, {1}, {2}, {0, 2}, {0, 1, 2}, {0}, {1}, {2}}
	} else {
		for i := range plan {
			plan[i] = []int{i % cfg.Programs}
		}
		counts := make([]int, cfg.Programs)
		for _, pms := range plan {
			for _, p := range pms {
				counts[p]++
			}
		}
		for i := 0; i < cfg.Programs; i++ {
			for counts[i] < 3 {
				epoch := i % epochs
				if len(plan[epoch]) < 2 {
					plan[epoch] = append(plan[epoch], i)
					counts[i]++
				} else {
					break
				}
			}
		}
	}
	return positions, plan, min(cfg.Bumps, epochs)
}

func makeLossPlan(cfg Config, positions []int) []lossWindow {
	if cfg.LossBursts == 0 || cfg.Programs < 3 {
		return nil
	}
	indexAt := func(i int) int {
		if len(positions) == 0 {
			return cfg.Packets / 4
		}
		return positions[min(i, len(positions)-1)] + 20
	}
	programs := makePrograms(cfg.Programs)
	groups := []lossWindow{
		{start: indexAt(1), end: indexAt(1) + 400, drops: map[uint16]int{programs[0].Streams[0].PID: 2, programs[0].Streams[1].PID: 1}},
		{start: indexAt(3), end: indexAt(3) + 400, drops: map[uint16]int{programs[1].Streams[0].PID: 3, programs[1].Streams[1].PID: 2}},
		{start: indexAt(5), end: indexAt(5) + 400, drops: map[uint16]int{programs[2].Streams[0].PID: 4, programs[2].Streams[1].PID: 1, programs[2].Streams[2].PID: 2}},
	}
	out := make([]lossWindow, 0, cfg.LossBursts)
	for i := 0; i < cfg.LossBursts; i++ {
		if i < len(groups) {
			out = append(out, groups[i])
			continue
		}
		pos := cfg.Packets * (i + 1) / (cfg.LossBursts + 1)
		def := programs[i%len(programs)].Streams[0]
		out = append(out, lossWindow{start: pos, end: pos + 400, drops: map[uint16]int{def.PID: 1}})
	}
	return out
}

func pmtChangeCounts(cfg Config, plan [][]int) map[uint16]int {
	out := make(map[uint16]int)
	for i := 0; i < cfg.Programs; i++ {
		out[uint16(i+1)] = 0
	}
	for _, pms := range plan {
		for _, index := range pms {
			out[uint16(index+1)]++
		}
	}
	return out
}

func countJumpPIDs(windows []lossWindow) int {
	seen := make(map[uint16]bool)
	for _, lw := range windows {
		for pid, count := range lw.drops {
			if count > 0 {
				seen[pid] = true
			}
		}
	}
	return len(seen)
}
