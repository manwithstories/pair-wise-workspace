package manifest

import (
	"os"
	"path/filepath"
	"sort"

	"relaycheck/internal/model"
)

// CrossCheck 对 HLS 与 DASH 两份清单做互查：
// 重复序号、缺失序号、引用文件缺失、双清单序号/时长/引用不一致，
// 并把合并后的分片元数据按序号输出。
func CrossCheck(dir string, hls, dash Playlist) model.ManifestReport {
	var rep model.ManifestReport
	hlsMap := make(map[int]model.SegmentRef, len(hls.Refs))
	dashMap := make(map[int]model.SegmentRef, len(dash.Refs))

	for _, r := range hls.Refs {
		if _, dup := hlsMap[r.Seq]; dup {
			rep.Faults = append(rep.Faults, model.Fault{
				Kind: model.FaultManifestDuplicate, Segment: r.Seq,
				Detail: "duplicate seq in HLS playlist",
			})
			continue
		}
		hlsMap[r.Seq] = r
	}
	for _, r := range dash.Refs {
		if _, dup := dashMap[r.Seq]; dup {
			rep.Faults = append(rep.Faults, model.Fault{
				Kind: model.FaultManifestDuplicate, Segment: r.Seq,
				Detail: "duplicate seq in DASH manifest",
			})
			continue
		}
		dashMap[r.Seq] = r
	}

	seqs := unionSeqs(hlsMap, dashMap)
	var prev int
	for i, seq := range seqs {
		if i > 0 && seq != prev+1 {
			for miss := prev + 1; miss < seq; miss++ {
				rep.Faults = append(rep.Faults, model.Fault{
					Kind: model.FaultManifestMissing, Segment: miss,
					Detail: "sequence gap between segments",
				})
			}
		}
		prev = seq

		hr, inHls := hlsMap[seq]
		dr, inDash := dashMap[seq]
		meta := model.SegmentMeta{Seq: seq, Start: hr.Start, Duration: hr.Duration}
		switch {
		case !inHls:
			meta = model.SegmentMeta{Seq: seq, Start: dr.Start, Duration: dr.Duration}
			rep.Faults = append(rep.Faults, model.Fault{
				Kind: model.FaultManifestMissing, Segment: seq,
				Detail: "segment present in DASH but absent from HLS",
			})
		case !inDash:
			rep.Faults = append(rep.Faults, model.Fault{
				Kind: model.FaultManifestMissing, Segment: seq,
				Detail: "segment present in HLS but absent from DASH",
			})
		default:
			if hr.URI != dr.URI {
				rep.Faults = append(rep.Faults, model.Fault{
					Kind: model.FaultManifestMismatch, Segment: seq,
					Detail: "HLS ref " + hr.URI + " != DASH ref " + dr.URI,
				})
			}
			if hr.Duration.IsValid() && dr.Duration.IsValid() &&
				hr.Duration.Cmp(dr.Duration) != 0 {
				rep.Faults = append(rep.Faults, model.Fault{
					Kind: model.FaultManifestMismatch, Segment: seq,
					Detail: "duration differs between HLS and DASH",
				})
			}
		}

		// 以统一文件名为准（两份清单引用一致时二者相同）。
		switch {
		case inHls:
			meta.File = hr.URI
		case inDash:
			meta.File = dr.URI
		}
		if meta.File != "" {
			if _, err := os.Stat(filepath.Join(dir, meta.File)); err != nil {
				meta.Exists = false
				rep.Faults = append(rep.Faults, model.Fault{
					Kind: model.FaultManifestMissing, Segment: seq,
					Detail: "referenced file not found: " + meta.File,
				})
			} else {
				meta.Exists = true
			}
		}
		rep.Segments = append(rep.Segments, meta)
	}
	return rep
}

func unionSeqs(a, b map[int]model.SegmentRef) []int {
	seen := make(map[int]struct{}, len(a)+len(b))
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	out := make([]int, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
