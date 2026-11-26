package mixer

import (
	"testing"

	"tsproof/internal/adapter"
	"tsproof/internal/clock"
	"tsproof/internal/fixture"
)

func mkBlock(track uint16, idx int, pts int64, key, untrust bool, kind adapter.BlockKind) *adapter.Block {
	return &adapter.Block{
		TrackID: track, Index: idx, PTS27: pts, Key: key,
		Untrusted: untrust, Kind: kind,
	}
}

func TestComposeSlotsAndAudioWindowing(t *testing.T) {
	vpid, apid := fixture.PIDVideo, fixture.PIDAudio
	blocks := map[uint16][]*adapter.Block{
		vpid: {
			mkBlock(vpid, 0, 100, true, false, adapter.KindImage),
			mkBlock(vpid, 1, 200, false, false, adapter.KindImage),
			mkBlock(vpid, 2, 400, true, false, adapter.KindImage),
		},
		apid: {
			mkBlock(apid, 0, 100, true, false, adapter.KindAudio),
			mkBlock(apid, 1, 150, true, false, adapter.KindAudio),
			mkBlock(apid, 2, 250, true, false, adapter.KindAudio),
			mkBlock(apid, 3, 400, true, false, adapter.KindAudio),
		},
	}
	plan := Compose(blocks, vpid, apid)
	if len(plan.Slots) != 3 {
		t.Fatalf("slots=%d want 3", len(plan.Slots))
	}
	// 槽0 [100,200)：音频 100、150。
	if len(plan.Slots[0].Audio) != 2 {
		t.Fatalf("slot0 audio=%d want 2", len(plan.Slots[0].Audio))
	}
	// 槽1 [200,400)：音频 250。
	if len(plan.Slots[1].Audio) != 1 {
		t.Fatalf("slot1 audio=%d want 1", len(plan.Slots[1].Audio))
	}
	// 槽2 [400, ...)：音频 400。
	if len(plan.Slots[2].Audio) != 1 {
		t.Fatalf("slot2 audio=%d want 1", len(plan.Slots[2].Audio))
	}
	if plan.OrphanAudio != 0 {
		t.Fatalf("orphan=%d want 0", plan.OrphanAudio)
	}
	times := plan.SlotTimes()
	want := []clock.Tick{100, 200, 400}
	for i := range want {
		if times[i] != want[i] {
			t.Fatalf("slot time[%d]=%d want %d", i, times[i], want[i])
		}
	}
}

func TestOrphanAudioBeforeFirstFrame(t *testing.T) {
	vpid, apid := fixture.PIDVideo, fixture.PIDAudio
	blocks := map[uint16][]*adapter.Block{
		vpid: {mkBlock(vpid, 0, 500, true, false, adapter.KindImage)},
		apid: {
			mkBlock(apid, 0, 100, true, false, adapter.KindAudio),
			mkBlock(apid, 1, 500, true, false, adapter.KindAudio),
		},
	}
	plan := Compose(blocks, vpid, apid)
	if plan.OrphanAudio != 1 {
		t.Fatalf("orphan=%d want 1", plan.OrphanAudio)
	}
	if len(plan.Slots[0].Audio) != 1 {
		t.Fatalf("slot audio=%d want 1", len(plan.Slots[0].Audio))
	}
}

func TestUntrustPropagatesToSlot(t *testing.T) {
	vpid, apid := fixture.PIDVideo, fixture.PIDAudio
	blocks := map[uint16][]*adapter.Block{
		vpid: {mkBlock(vpid, 0, 100, true, false, adapter.KindImage)},
		apid: {mkBlock(apid, 0, 100, true, true, adapter.KindAudio)},
	}
	plan := Compose(blocks, vpid, apid)
	if !plan.Slots[0].Untrusted {
		t.Fatal("untrusted audio must mark slot untrusted")
	}
}

func TestEmptyVideoPlan(t *testing.T) {
	apid := fixture.PIDAudio
	blocks := map[uint16][]*adapter.Block{
		apid: {mkBlock(apid, 0, 100, true, false, adapter.KindAudio)},
	}
	plan := Compose(blocks, fixture.PIDVideo, apid)
	if len(plan.Slots) != 0 || plan.OrphanAudio != 1 {
		t.Fatalf("empty video: slots=%d orphan=%d", len(plan.Slots), plan.OrphanAudio)
	}
}

func TestVFRTimingPreserved(t *testing.T) {
	// 不等间隔帧：槽位时刻必须严格沿用帧 PTS，不被重采样为均匀间隔。
	vpid := fixture.PIDVideo
	pts := []int64{0, 900_000, 2_700_000, 3_600_000} // 间隔 0.9M/1.8M/0.9M
	blocks := map[uint16][]*adapter.Block{vpid: {}}
	for i, p := range pts {
		blocks[vpid] = append(blocks[vpid],
			mkBlock(vpid, i, p, i%2 == 0, false, adapter.KindImage))
	}
	plan := Compose(blocks, vpid, fixture.PIDAudio)
	for i, s := range plan.Slots {
		if int64(s.At) != pts[i] {
			t.Fatalf("VFR slot[%d]=%d want %d", i, s.At, pts[i])
		}
	}
}
