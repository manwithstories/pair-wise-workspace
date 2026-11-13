import unittest
from fractions import Fraction

from tlrender.timebase import TimeBase, validate_ms_interval


class TimeBaseTests(unittest.TestCase):
    def test_rational_parse(self):
        tb = TimeBase.parse("24000/1001")
        self.assertEqual(tb.fps, Fraction(24000, 1001))
        self.assertEqual(tb.fps_text, "24000/1001")

    def test_frame_to_ms_known_values(self):
        tb = TimeBase.parse("24000/1001")
        self.assertEqual(tb.frame_to_ms(0), 0)
        self.assertEqual(tb.frame_to_ms(24), 1001)
        self.assertEqual(tb.frame_to_ms(240), 10010)

    def test_half_up_rounding(self):
        tb = TimeBase.parse("3")
        self.assertEqual(tb.frame_to_ms(1), 333)
        tb2 = TimeBase.parse("8")
        self.assertEqual(tb2.frame_to_ms(1), 125)

    def test_round_trip_error_under_one_ms(self):
        tb = TimeBase.parse("24000/1001")
        for frame in range(0, 24000, 37):
            ms = tb.frame_to_ms(frame)
            back = tb.ms_to_frame(ms)
            self.assertLessEqual(abs(back - frame), 1)
            exact = float(Fraction(frame) * 1000 / tb.fps)
            self.assertLess(abs(ms - exact), 1.0)

    def test_invalid_fps_and_frames(self):
        with self.assertRaises(ValueError):
            TimeBase.parse("0/1")
        with self.assertRaises(ValueError):
            TimeBase.parse("24/0")
        tb = TimeBase.parse("24")
        with self.assertRaises(ValueError):
            tb.frame_to_ms(-1)

    def test_span_validation(self):
        tb = TimeBase.parse("24")
        self.assertEqual(tb.frame_span_to_ms(0, 24), (0, 1000))
        with self.assertRaises(ValueError):
            tb.frame_span_to_ms(10, 10)
        with self.assertRaises(ValueError):
            tb.frame_span_to_ms(20, 10)

    def test_validate_ms_interval(self):
        validate_ms_interval(0, 100)
        with self.assertRaises(ValueError):
            validate_ms_interval(5, 5)
        with self.assertRaises(ValueError):
            validate_ms_interval(9, 3)
        with self.assertRaises(ValueError):
            validate_ms_interval(-1, 3)


if __name__ == "__main__":
    unittest.main()
