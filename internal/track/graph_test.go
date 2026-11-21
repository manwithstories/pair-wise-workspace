package track

import (
	"testing"

	"mkvarchive/internal/model"
)

func mkTrack(uid uint64, off int64, join int64) *model.Track {
	return &model.Track{UID: uid, Number: uid, Kind: model.KindVideo,
		HeaderOffset: off, JoinUID: join}
}

func TestBuildGraphRejectsDuplicateUID(t *testing.T) {
	_, err := BuildGraph([]*model.Track{mkTrack(7, 10, -1), mkTrack(7, 30, -1)})
	ae, ok := err.(*model.ArchiveError)
	if !ok || ae.Code != "TRACK_DUPLICATE_UID" || ae.Offset != 30 {
		t.Fatalf("want duplicate anchored at 30, got %v", err)
	}
}

func TestBuildGraphRejectsMissingDependency(t *testing.T) {
	_, err := BuildGraph([]*model.Track{mkTrack(1, 5, 99)})
	ae, ok := err.(*model.ArchiveError)
	if !ok || ae.Code != "TRACK_MISSING_DEPENDENCY" || ae.Offset != 5 {
		t.Fatalf("want missing dependency at 5, got %v", err)
	}
}

func TestBuildGraphRejectsCycle(t *testing.T) {
	tracks := []*model.Track{
		mkTrack(1, 100, 2),
		mkTrack(2, 200, 1),
	}
	_, err := BuildGraph(tracks)
	ae, ok := err.(*model.ArchiveError)
	if !ok || ae.Code != "TRACK_DEPENDENCY_CYCLE" {
		t.Fatalf("want cycle error, got %v", err)
	}
	if ae.Offset != 100 {
		t.Fatalf("cycle must anchor at earliest offset, got %d", ae.Offset)
	}
}

func TestDependencyOrder(t *testing.T) {
	// 3 joins 1 and 2; 1 joins nothing. Valid order: 2, 1, 3 (deps first,
	// ties in declaration order).
	tracks := []*model.Track{
		mkTrack(3, 30, -1),
		mkTrack(1, 10, -1),
		mkTrack(2, 20, -1),
	}
	tracks[0].JoinUID = 1
	g, err := BuildGraph(tracks)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	g.edges[3] = []uint64{1, 2}
	order := g.DependencyOrder()
	pos := map[uint64]int{}
	for i, uid := range order {
		pos[uid] = i
	}
	if pos[1] >= pos[3] || pos[2] >= pos[3] {
		t.Fatalf("dependencies not ordered first: %v", order)
	}
}
