// Package source 扫描分片目录，对照 ladder.json 标记缺口、重叠与残片。
package source

import (
	"encoding/json"
	"strconv"
	"os"
	"path/filepath"
	"strings"

	"dashpack/internal/errs"
)

// 问题类型。
const (
	IssueGap     = "gap"     // 缺口：声明的分片在目录中缺失
	IssueOverlap = "overlap" // 重叠：分片起点早于前片终点
	IssuePartial = "partial" // 残片：片长与标称不符
	IssueOffset  = "offset"  // 偏移：首片起点不在公共原点
)

// Ladder 是 ladder.json 的顶层结构，由夹具生成器写入。
type Ladder struct {
	Seed int64      `json:"seed"`
	Reps []LadderRep `json:"reps"`
}

// LadderRep 描述一路表示层的标称参数与逐片记录。
type LadderRep struct {
	ID          string `json:"id"`
	ContentType string `json:"content_type"`
	Bandwidth   int64  `json:"bandwidth"`
	Codecs      string `json:"codecs"`
	Timescale   int64  `json:"timescale"`
	NominalMs   int64  `json:"nominal_ms"` // 单片标称时长
	GopMs       int64  `json:"gop_ms"`     // GOP 元数据（1s/2s 混用）
	InitFile    string `json:"init_file"`
	MediaTemplate string `json:"media_template"`
	InitTemplate  string `json:"init_template"`
	Segments    []LadderSegment `json:"segments"`
}

// LadderSegment 是 ladder 中的单片记录。
type LadderSegment struct {
	Index      int64 `json:"index"`
	File       string `json:"file"`
	DurationMs int64 `json:"duration_ms"`
	OffsetMs   int64 `json:"offset_ms"` // 相对本路时间轴原点的起始偏移
	FilledFrom string `json:"filled_from,omitempty"` // tolerant 补缺来源文件
}

// Segment 是扫描后的单片状态。
type Segment struct {
	LadderSegment
	Present bool
}

// Issue 描述扫描发现的一处异常。
type Issue struct {
	RepID  string `json:"rep_id"`
	Kind   string `json:"kind"`
	Index  int64  `json:"index"`
	Detail string `json:"detail"`
}

// Rep 是一路表示层的扫描结果。
type Rep struct {
	LadderRep
	Dir      string
	Segments []Segment
	Issues   []Issue
}

// SafeJoin 拼接路径并确保不逃出 root。
func SafeJoin(root, name string) (string, error) {
	if strings.ContainsAny(name, "/\\") || name == ".." || name == "" {
		return "", errs.New(errs.ESource, errs.ExitUsage, "unsafe path element %q", name)
	}
	p := filepath.Join(root, name)
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", errs.New(errs.ESource, errs.ExitUsage, "resolve root: %v", err)
	}
	pAbs, err := filepath.Abs(p)
	if err != nil {
		return "", errs.New(errs.ESource, errs.ExitUsage, "resolve path: %v", err)
	}
	if pAbs != rootAbs && !strings.HasPrefix(pAbs, rootAbs+string(os.PathSeparator)) {
		return "", errs.New(errs.ESource, errs.ExitUsage, "path %q escapes source root", name)
	}
	return pAbs, nil
}

// LoadLadder 读取 source 根目录下的 ladder.json。
func LoadLadder(root string) (*Ladder, error) {
	data, err := os.ReadFile(filepath.Join(root, "ladder.json"))
	if os.IsNotExist(err) {
		return nil, errs.New(errs.ESource, errs.ExitUsage, "ladder.json not found in %s", root)
	}
	if err != nil {
		return nil, errs.New(errs.ESource, errs.ExitUsage, "read ladder.json: %v", err)
	}
	var l Ladder
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, errs.New(errs.ESource, errs.ExitUsage, "parse ladder.json: %v", err)
	}
	return &l, nil
}

// Scan 扫描 source 目录，对照 ladder 标记缺口、重叠、残片与偏移。
func Scan(root string) ([]Rep, error) {
	l, err := LoadLadder(root)
	if err != nil {
		return nil, err
	}
	if len(l.Reps) == 0 {
		return nil, errs.New(errs.EEmpty, errs.ExitUsage, "no representations in ladder.json")
	}
	var reps []Rep
	foundAny := false
	for _, lr := range l.Reps {
		dir, err := SafeJoin(root, lr.ID)
		if err != nil {
			return nil, err
		}
		rep := Rep{LadderRep: lr, Dir: dir}
		nominal := lr.NominalMs
		var prevEnd int64 = -1
		var cursor int64 // 累计时长游标（不含首片偏移）
		for _, ls := range lr.Segments {
			if _, err := SafeJoin(dir, ls.File); err != nil {
				return nil, err
			}
			seg := Segment{LadderSegment: ls}
			p := filepath.Join(dir, ls.File)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				seg.Present = true
				foundAny = true
			} else {
				rep.Issues = append(rep.Issues, Issue{RepID: lr.ID, Kind: IssueGap,
					Index: ls.Index, Detail: "segment file missing: " + ls.File})
			}
			start := ls.OffsetMs + cursor
			if ls.Index == 1 && ls.OffsetMs != 0 {
				rep.Issues = append(rep.Issues, Issue{RepID: lr.ID, Kind: IssueOffset,
					Index: ls.Index, Detail: "first segment starts at +" +
						itoa(ls.OffsetMs) + "ms"})
			}
			if prevEnd >= 0 && start < prevEnd {
				rep.Issues = append(rep.Issues, Issue{RepID: lr.ID, Kind: IssueOverlap,
					Index: ls.Index, Detail: "segment starts before previous ends"})
			}
			if seg.Present && ls.DurationMs != nominal {
				rep.Issues = append(rep.Issues, Issue{RepID: lr.ID, Kind: IssuePartial,
					Index: ls.Index, Detail: "duration " + itoa(ls.DurationMs) +
						"ms != nominal " + itoa(nominal) + "ms"})
			}
			prevEnd = start + ls.DurationMs
			cursor += ls.DurationMs
			rep.Segments = append(rep.Segments, seg)
		}
		reps = append(reps, rep)
	}
	if !foundAny {
		return nil, errs.New(errs.EEmpty, errs.ExitUsage, "no segment files under %s", root)
	}
	return reps, nil
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
// SafeJoinRel 拼接相对路径（可含子目录）并确保不逃出 root。
func SafeJoinRel(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", errs.New(errs.ESource, errs.ExitUsage, "unsafe path %q", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", errs.New(errs.ESource, errs.ExitUsage, "path %q escapes source root", rel)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", errs.New(errs.ESource, errs.ExitUsage, "resolve root: %v", err)
	}
	pAbs, err := filepath.Abs(filepath.Join(rootAbs, clean))
	if err != nil {
		return "", errs.New(errs.ESource, errs.ExitUsage, "resolve path: %v", err)
	}
	if pAbs != rootAbs && !strings.HasPrefix(pAbs, rootAbs+string(os.PathSeparator)) {
		return "", errs.New(errs.ESource, errs.ExitUsage, "path %q escapes source root", rel)
	}
	return pAbs, nil
}
