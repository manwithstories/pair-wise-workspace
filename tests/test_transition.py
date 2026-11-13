import unittest

from tlrender.overlap import resolve_overlaps
from tlrender.tl_parse import Cue, Transition
from tlrender.transition import apply_transitions


def cue(cid, start, end):
    return Cue(id=cid, track=1, start=start, end=end, text=cid,
               source="mem", line=1)


class TransitionTests(unittest.TestCase):
    def _prepare(self, cues):
        for c in resolve_overlaps(cues):
            pass
        return cues

    def test_cut_zero_width_trim(self):
        c = cue("c", 27200, 28800)
        cut = Transition("cut", 28000, 28000, "mem", 2)
        out = apply_transitions([c], [cut])[0]
        self.assertEqual((out.final_start, out.final_end), (27200, 28000))
        self.assertEqual(out.moved, 1)
        self.assertEqual(out.resolved, 0)

    def test_dissolve_trim_right(self):
        c = cue("c", 28800, 30800)
        tr = Transition("dissolve", 29500, 30500, "mem", 3)
        out = apply_transitions([c], [tr])[0]
        self.assertEqual((out.final_start, out.final_end), (28800, 29500))

    def test_short_cue_is_translated_not_crushed(self):
        c = cue("c", 31800, 33300)
        tr = Transition("wipe", 32000, 33000, "mem", 4)
        out = apply_transitions([c], [tr])[0]
        self.assertEqual(out.final_end - out.final_start, 1500)
        self.assertGreaterEqual(out.final_start, 33000)
        self.assertEqual(out.moved, 1)

    def test_no_straddling_after_multiple_windows(self):
        c = cue("c", 1000, 5000)
        windows = [
            Transition("dissolve", 2000, 2400, "mem", 1),
            Transition("wipe", 3600, 4000, "mem", 2),
        ]
        out = apply_transitions([c], windows)[0]
        for win in windows:
            self.assertFalse(
                out.final_start < win.end and win.start < out.final_end
            )

    def test_dropped_cue_skipped(self):
        c = cue("c", 0, 100)
        c.dropped = True
        c.final_start, c.final_end = 0, 100
        tr = Transition("cut", 50, 50, "mem", 1)
        out = apply_transitions([c], [tr])[0]
        self.assertEqual((out.final_start, out.final_end), (0, 100))


if __name__ == "__main__":
    unittest.main()
