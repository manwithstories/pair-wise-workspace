package mp4

import (
	"errors"
	"fmt"
	"io"
	"os"

	gomp4 "github.com/abema/go-mp4"

	"relaycheck/internal/model"
)

// moofData 汇总一个 moof 盒内与样本相关的字段。
type moofData struct {
	baseDecodeTime   uint64
	haveTfdt         bool
	trackID          uint32
	defaultDuration  uint32
	defaultSize      uint32
	haveDefaultSize  bool
	defaultFlags     uint32
	haveDefaultFlags bool
	truns            []gomp4.Trun
}

// ParseFragment 流式解析单个 fMP4 分片文件。
//
// 文件按顶层盒逐个扫描：moof 解析 tfdt/trun，mdat 校验样本越界。
// 盒长度越界时记录首个错误偏移，并尝试跳过该盒继续处理可恢复部分；
// timeScale<=0、样本超出 mdat 等同样登记为 Fault，不中断其它分片。
func ParseFragment(path string, seq int, timeScale uint32, mem *model.MemTracker) (model.FragmentReport, error) {
	rep := model.FragmentReport{Seq: seq, TimeScale: timeScale, FirstBadOff: -1}

	f, err := os.Open(path)
	if err != nil {
		return rep, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return rep, err
	}
	fileLen := st.Size()
	if mem != nil {
		if err := mem.Acquire(fileLen); err != nil {
			return rep, markOversized(&rep, fileLen, mem)
		}
		defer mem.Release(fileLen)
	}

	lr := &limitedReader{r: f, base: 0, end: fileLen, pos: 0}
	var moof *moofData
	var mdatBox *boxInfo
	off := int64(0)
	for off < fileLen {
		b, err := lr.nextBox(off)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			rep.Faults = appendFault(rep.Faults, model.Fault{
				Kind: model.FaultBoxTruncated, Segment: seq, Offset: b.off,
				Detail: err.Error(),
			}, &rep.FirstBadOff)
			return rep, nil // 顶层盒无法可靠定位后续边界，本分片到此为止
		}
		switch b.typ {
		case "moof":
			raw, rerr := lr.readBoxBytes(b)
			if rerr != nil {
				rep.Faults = appendFault(rep.Faults, model.Fault{
					Kind: model.FaultBoxTruncated, Segment: seq, Offset: b.off,
					Detail: rerr.Error(),
				}, &rep.FirstBadOff)
			} else if md, perr := parseMoof(raw); perr != nil {
				rep.Faults = appendFault(rep.Faults, model.Fault{
					Kind: model.FaultBoxTruncated, Segment: seq, Offset: b.off,
					Detail: "moof payload: " + perr.Error(),
				}, &rep.FirstBadOff)
			} else {
				moof = md
			}
		case "mdat":
			bc := b
			mdatBox = &bc
		}
		off = b.off + int64(b.size)
	}

	if timeScale <= 0 {
		rep.Faults = appendFault(rep.Faults, model.Fault{
			Kind: model.FaultTimeScaleZero, Segment: seq,
			Detail: fmt.Sprintf("time scale = %d", timeScale),
		}, &rep.FirstBadOff)
		return rep, nil
	}
	if moof == nil {
		rep.Faults = appendFault(rep.Faults, model.Fault{
			Kind: model.FaultBoxTruncated, Segment: seq,
			Detail: "no moof box found",
		}, &rep.FirstBadOff)
		return rep, nil
	}
	buildSamples(&rep, seq, moof, timeScale, mdatBox)
	return rep, nil
}

func markOversized(rep *model.FragmentReport, size int64, mem *model.MemTracker) error {
	rep.FirstBadOff = 0
	rep.Faults = append(rep.Faults, model.Fault{
		Kind:   model.FaultOversized,
		Segment: rep.Seq,
		Detail: fmt.Sprintf("fragment size %d exceeds memory limit %d", size, mem.Limit()),
	})
	return model.ErrMemoryLimit
}

func appendFault(faults []model.Fault, f model.Fault, firstBad *int64) []model.Fault {
	if *firstBad < 0 {
		*firstBad = f.Offset
	}
	return append(faults, f)
}
