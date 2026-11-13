import tempfile
import unittest
from pathlib import Path

from tlrender.tl_parse import (
    CueTooLong,
    ParseError,
    TimelineEncoding,
    TimelineMissing,
    load_timeline,
    parse_text,
)

V2 = """@timeline v2
fps 24000/1001
track 1
cue a 0 24 "hello"
cue b 12 48 pri 3 "world"
transition cut 60 60
"""

V1 = """@timeline v1
head "x"
shot s1 0 2000
cue a 0 1000 "hi"
transition cut 2000 2000
"""


class ParseTests(unittest.TestCase):
    def test_v2_basic(self):
        result = parse_text(V2, "mem")
        self.assertEqual(len(result.cues), 2)
        self.assertEqual(result.cues[0].start_frame, 0)
        self.assertEqual(result.cues[0].fps, "24000/1001")
        self.assertEqual(result.transitions[0].kind, "cut")
        self.assertEqual(result.cues[1].priority, 3)

    def test_v1_basic(self):
        result = parse_text(V1, "mem")
        self.assertEqual(result.cues[0].track, 3)
        self.assertEqual(result.cues[0].start, 0)
        self.assertEqual(result.transitions[0].end, 2000)

    def test_structural_error_rejects_whole_file(self):
        bad = V2 + 'cue c 100 "missing end frame"\n'
        with self.assertRaises(ParseError) as ctx:
            parse_text(bad, "sample.tl")
        self.assertEqual(ctx.exception.source, "sample.tl")
        self.assertGreaterEqual(ctx.exception.line, 1)

    def test_unknown_statement(self):
        with self.assertRaises(ParseError):
            parse_text("@timeline v2\nfps 24\ntrack 1\nwat 1\n", "x")

    def test_duplicate_id_structural(self):
        bad = V2 + 'cue a 100 120 "again"\n'
        with self.assertRaises(ParseError):
            parse_text(bad, "x")

    def test_zero_duration_is_warning_skip(self):
        text = "@timeline v2\nfps 24\ntrack 1\ncue z 10 10 \"zero\"\ncue y 0 5 \"ok\"\n"
        result = parse_text(text, "x")
        self.assertEqual([c.id for c in result.cues], ["y"])
        self.assertEqual(result.warnings[0].code, "invalid_interval")

    def test_negative_ms_warning_v1(self):
        text = "@timeline v1\nshot s 0 100\ncue z -5 50 \"neg\"\n"
        result = parse_text(text, "x")
        self.assertEqual(result.cues, [])
        self.assertEqual(result.warnings[0].code, "invalid_interval")

    def test_cue_too_long_warning(self):
        long_ms = 10 * 60 * 1000 + 1
        text = f"@timeline v1\nshot s 0 {long_ms + 100}\ncue z 0 {long_ms} \"long\"\n"
        result = parse_text(text, "x")
        self.assertEqual(result.cues, [])
        self.assertEqual(result.warnings[0].code, "cue_too_long")

    def test_missing_file_exit_one(self):
        with self.assertRaises(TimelineMissing) as ctx:
            load_timeline("/no/such/file-12345.tl")
        self.assertEqual(ctx.exception.exit_code, 1)

    def test_non_utf8_exit_one(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "bad.tl"
            path.write_bytes(b"@timeline v2\n\xff\xfe\x00bad")
            with self.assertRaises(TimelineEncoding) as ctx:
                load_timeline(path)
            self.assertEqual(ctx.exception.exit_code, 1)

    def test_cue_too_long_exit_two(self):
        long_ms = 10 * 60 * 1000 + 5
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "long.tl"
            path.write_text(
                f"@timeline v1\nshot s 0 {long_ms + 100}\ncue z 0 {long_ms} \"x\"\n",
                encoding="utf-8",
            )
            result = load_timeline(path)
            self.assertTrue(any(d.code == "cue_too_long" for d in result.diagnostics))
            self.assertEqual(CueTooLong.exit_code, 2)

    def test_file_size_limit(self):
        from tlrender import MAX_FILE_BYTES
        from tlrender.tl_parse import TimelineTooLarge

        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "big.tl"
            path.write_bytes(b"#" * (MAX_FILE_BYTES + 1))
            with self.assertRaises(TimelineTooLarge):
                load_timeline(path)


if __name__ == "__main__":
    unittest.main()
