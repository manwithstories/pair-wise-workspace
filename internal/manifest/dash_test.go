package manifest

import (
	"strings"
	"testing"

	"mediaverify/internal/clock"
	"mediaverify/internal/fault"
)

const goodMPD = `<?xml version="1.0"?>
<MPD>
  <Period>
    <AdaptationSet>
      <Representation id="a1" bandwidth="128000">
        <SegmentTemplate media="seg_$Number$.m4s" initialization="init.mp4" timescale="48000">
          <SegmentTimeline>
            <S t="0" d="96000" r="1"/>
            <S d="48000"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
      <Representation id="a2" bandwidth="256000">
        <SegmentTemplate media="hi_$Number$.m4s" initialization="init_hi.mp4" timescale="48000">
          <SegmentTimeline>
            <S t="0" d="120000" r="1"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

func TestParseDASHGood(t *testing.T) {
	p, err := ParseDASH("good.mpd", strings.NewReader(goodMPD))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Representations) != 2 {
		t.Fatalf("reps=%d", len(p.Representations))
	}
	r0 := p.Representations[0]
	if len(r0.Segments) != 3 {
		t.Fatalf("segs=%d", len(r0.Segments))
	}
	// 段边界：0-2s, 2-4s, 4-5s
	want := [][2]clock.Ticks{{0, 2 * clock.Second}, {2 * clock.Second, 2 * clock.Second}, {4 * clock.Second, clock.Second}}
	for i, s := range r0.Segments {
		if s.Start != want[i][0] || s.Duration != want[i][1] {
			t.Fatalf("seg %d: start=%d dur=%d", i, s.Start, s.Duration)
		}
	}
	if r0.Segments[1].URI != "seg_2.m4s" {
		t.Fatalf("uri=%s", r0.Segments[1].URI)
	}
	if p.Representations[1].Bandwidth != 256000 {
		t.Fatalf("bandwidth=%d", p.Representations[1].Bandwidth)
	}
}

func TestParseDASHNonMonotonic(t *testing.T) {
	bad := strings.Replace(goodMPD, `<S t="0" d="96000" r="1"/>`,
		"<S t=\"96000\" d=\"96000\"/>\n            <S t=\"48000\" d=\"96000\"/>", 1)
	_, err := ParseDASH("bad.mpd", strings.NewReader(bad))
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindManifest {
		t.Fatalf("err=%v", err)
	}
	if fe.Line == 0 {
		t.Fatal("expected line position")
	}
	if !strings.Contains(fe.Msg, "non-monotonic") {
		t.Fatalf("msg=%s", fe.Msg)
	}
}

func TestParseDASHMissingInit(t *testing.T) {
	bad := strings.Replace(goodMPD, ` initialization="init.mp4"`, "", 1)
	_, err := ParseDASH("bad.mpd", strings.NewReader(bad))
	fe, ok := fault.As(err)
	if !ok || !strings.Contains(fe.Msg, "initialization") {
		t.Fatalf("err=%v", err)
	}
}

func TestParseDASHOverlap(t *testing.T) {
	// t 回退造成重叠：第一段 0-96000，第二段 t=48000 与前者重叠。
	bad := strings.Replace(goodMPD, `<S t="0" d="96000" r="1"/>`,
		`<S t="0" d="96000"/>`+"\n            "+`<S t="48000" d="96000"/>`, 1)
	// 非单调检查先触发？t=48000 < cur=96000 → 报非单调；改用相等后重叠场景：
	// 第一段 d=96000，第二段 t=96000 d=96000，第三段 t=150000（< 192000 且 > 96000+96000? 不重叠）
	// 直接构造：t 单调但段重叠不可能（cur 推进等于 t+d）。改为手工 XML。
	_ = bad
	overlap := `<?xml version="1.0"?>
<MPD><Period><AdaptationSet>
  <Representation id="x" bandwidth="1">
    <SegmentTemplate media="s$Number$.m4s" initialization="i.mp4" timescale="1000">
      <SegmentTimeline>
        <S t="0" d="2000"/>
        <S t="1000" d="2000"/>
      </SegmentTimeline>
    </SegmentTemplate>
  </Representation>
</AdaptationSet></Period></MPD>`
	_, err := ParseDASH("ov.mpd", strings.NewReader(overlap))
	// t=1000 < cur=2000 → 非单调错误（重叠的成因即时间回退）
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindManifest {
		t.Fatalf("err=%v", err)
	}
}
