import unittest

from tlrender.overlap import count_dropped, count_resolved, resolve_overlaps
from tlrender.tl_parse import Cue


def make(cid, start, end, track=1, priority=0):
    return Cue(
        id=cid, track=track, start=start, end=end,
        text=cid, source="mem", line=1, priority=priority,
    )


class OverlapTests(unittest.TestCase):
    def test_touching_endpoints_not_overlap(self):
        cues = resolve_overlaps([make("a", 0, 500), make("b", 500, 1000)])
        self.assertEqual([(c.final_start, c.final_end) for c in cues],
                         [(0, 500), (500, 1000)])
        self.assertEqual(count_resolved(cues), 0)

    def test_half_open_overlap_high_priority_kept(self):
        hi = make("hi", 400, 1200, priority=5)
        lo = make("lo", 0, 1000, priority=1)
        cues = {c.id: c for c in resolve_overlaps([lo, hi])}
        self.assertEqual((cues["hi"].final_start, cues["hi"].final_end), (400, 1200))
        self.assertEqual((cues["lo"].final_start, cues["lo"].final_end), (0, 400))
        self.assertEqual(cues["lo"].resolved, 1)
        self.assertEqual(cues["hi"].resolved, 0)

    def test_short_remainder_dropped(self):
        hi = make("hi", 1000, 10200, priority=9)
        lo = make("lo", 9600, 10500, priority=2)
        cues = {c.id: c for c in resolve_overlaps([hi, lo])}
        self.assertTrue(cues["lo"].dropped)
        self.assertLess(cues["lo"].final_end - cues["lo"].final_start, 400)
        self.assertEqual(count_dropped(list(cues.values())), 1)

    def test_different_tracks_never_resolved(self):
        cues = resolve_overlaps([
            make("a", 0, 1000, track=1),
            make("b", 500, 1500, track=2),
        ])
        self.assertEqual(count_resolved(cues), 0)

    def test_stable_ordering(self):
        cues = resolve_overlaps([
            make("z", 0, 1000),
            make("a", 500, 1500),
        ])
        self.assertEqual([c.id for c in cues], ["z", "a"])
        self.assertEqual(cues[1].final_start, 1000)

    def test_three_cue_chain(self):
        cues = resolve_overlaps([
            make("a", 0, 1500, priority=2),
            make("b", 1200, 1600, priority=8),
            make("c", 1500, 2400, priority=3),
        ])
        by_id = {c.id: c for c in cues}
        self.assertEqual((by_id["b"].final_start, by_id["b"].final_end),
                         (1200, 1600))
        self.assertEqual(by_id["a"].final_end, 1200)
        self.assertEqual(by_id["c"].final_start, 1600)
        self.assertEqual(by_id["a"].resolved, 1)
        self.assertEqual(by_id["c"].resolved, 1)
        self.assertEqual(by_id["b"].resolved, 0)


if __name__ == "__main__":
    unittest.main()
