"""Timeline validation: references, overlap, gaps, bounds and transitions."""

from __future__ import annotations

import unittest
from fractions import Fraction

from avtimeline.timeline import (
    TimelineParseError,
    parse_timeline,
    timeline_to_json,
    validate_timeline,
)


def codes(issues) -> list[str]:
    return [issue.code for issue in issues]


class TimelineParsingTests(unittest.TestCase):
    def test_rational_bounds(self) -> None:
        timeline = parse_timeline(
            {
                "window": {"start": 0, "end": "101/50"},
                "tracks": [
                    {"id": "cam", "clips": [{"id": "a", "start": "0", "end": "1/3"}]}
                ],
            }
        )
        clip = timeline.tracks["cam"].clips[0]
        self.assertEqual(clip.end, Fraction(1, 3))

    def test_missing_tracks_rejected(self) -> None:
        with self.assertRaises(TimelineParseError):
            parse_timeline({"window": {"start": 0, "end": 1}})

    def test_end_before_start_rejected(self) -> None:
        with self.assertRaises(TimelineParseError):
            parse_timeline(
                {
                    "tracks": [
                        {"id": "cam", "clips": [{"id": "a", "start": 2, "end": 1}]}
                    ]
                }
            )

    def test_roundtrip_serialization(self) -> None:
        document = {
            "window": {"start": 0, "end": 2},
            "tracks": [
                {
                    "id": "cam",
                    "clips": [{"id": "a", "start": 0, "end": 2, "source_in": "1/2"}],
                }
            ],
        }
        timeline = parse_timeline(document)
        payload = timeline_to_json(timeline)
        self.assertEqual(payload["tracks"][0]["clips"][0]["source_in"], "1/2")
        self.assertEqual(payload["window"], {"start": "0", "end": "2"})


class TrackValidationTests(unittest.TestCase):
    def test_overlap_reported_at_second_clip(self) -> None:
        timeline = parse_timeline(
            {
                "window": {"start": 0, "end": 4},
                "tracks": [
                    {
                        "id": "cam",
                        "clips": [
                            {"id": "a", "start": 0, "end": 3},
                            {"id": "b", "start": 2, "end": 4},
                        ],
                    }
                ],
            }
        )
        issues = validate_timeline(timeline)
        overlap = [i for i in issues if i.code == "clip_overlap"]
        self.assertEqual(len(overlap), 1)
        self.assertEqual(overlap[0].location.pts, Fraction(2))

    def test_duplicate_clip_id(self) -> None:
        timeline = parse_timeline(
            {
                "window": {"start": 0, "end": 4},
                "tracks": [
                    {
                        "id": "cam",
                        "clips": [
                            {"id": "a", "start": 0, "end": 1},
                            {"id": "a", "start": 2, "end": 3},
                        ],
                    }
                ],
            }
        )
        self.assertIn("duplicate_clip", codes(validate_timeline(timeline)))

    def test_gap_reported_at_exact_time(self) -> None:
        timeline = parse_timeline(
            {
                "window": {"start": 0, "end": 4},
                "tracks": [
                    {
                        "id": "mic",
                        "clips": [
                            {"id": "a", "start": 0, "end": 3, "source_in": 0},
                        ],
                    }
                ],
            }
        )
        issues = [i for i in validate_timeline(timeline) if i.code == "coverage_gap"]
        self.assertEqual(len(issues), 1)
        self.assertEqual(issues[0].location.pts, Fraction(3))
        self.assertEqual(issues[0].context["gap_end"], "4")

    def test_clip_beyond_source(self) -> None:
        timeline = parse_timeline(
            {
                "window": {"start": 0, "end": 4},
                "tracks": [
                    {"id": "cam", "clips": [{"id": "a", "start": 0, "end": 4}]}
                ],
            }
        )
        issues = validate_timeline(timeline, {"cam": Fraction(3)})
        beyond = [i for i in issues if i.code == "clip_beyond_source"]
        self.assertEqual(len(beyond), 1)
        self.assertEqual(beyond[0].location.pts, Fraction(0))

    def test_empty_track(self) -> None:
        timeline = parse_timeline(
            {"window": {"start": 0, "end": 1}, "tracks": [{"id": "empty"}]}
        )
        self.assertIn("empty_track", codes(validate_timeline(timeline)))

    def test_clip_outside_window(self) -> None:
        timeline = parse_timeline(
            {
                "window": {"start": 1, "end": 2},
                "tracks": [
                    {"id": "cam", "clips": [{"id": "a", "start": 0, "end": 2}]}
                ],
            }
        )
        self.assertIn("clip_outside_window", codes(validate_timeline(timeline)))


class TransitionTests(unittest.TestCase):
    def base(self, start: str, end: str):
        return parse_timeline(
            {
                "window": {"start": 0, "end": 4},
                "tracks": [
                    {
                        "id": "cam",
                        "clips": [
                            {"id": "a", "start": 0, "end": 2},
                            {"id": "b", "start": 2, "end": 4},
                        ],
                        "transitions": [
                            {"after": "a", "before": "b", "start": start, "end": end}
                        ],
                    }
                ],
            }
        )

    def test_covering_transition_ok(self) -> None:
        self.assertEqual(codes(validate_timeline(self.base("3/2", "5/2"))), [])

    def test_transition_missing_the_cut(self) -> None:
        issues = [
            i
            for i in validate_timeline(self.base("5/2", "7/2"))
            if i.code == "transition_span_uncovered"
        ]
        self.assertEqual(len(issues), 1)
        self.assertEqual(issues[0].location.pts, Fraction(2))

    def test_transition_not_adjacent(self) -> None:
        timeline = parse_timeline(
            {
                "window": {"start": 0, "end": 4},
                "tracks": [
                    {
                        "id": "cam",
                        "clips": [
                            {"id": "a", "start": 0, "end": 1},
                            {"id": "b", "start": 2, "end": 4},
                        ],
                        "transitions": [
                            {"after": "a", "before": "b", "start": "1", "end": "3"}
                        ],
                    }
                ],
            }
        )
        self.assertIn("transition_not_adjacent", codes(validate_timeline(timeline)))

    def test_unknown_clip_reference(self) -> None:
        timeline = parse_timeline(
            {
                "window": {"start": 0, "end": 2},
                "tracks": [
                    {
                        "id": "cam",
                        "clips": [{"id": "a", "start": 0, "end": 2}],
                        "transitions": [
                            {"after": "ghost", "before": "a", "start": 0, "end": 1}
                        ],
                    }
                ],
            }
        )
        self.assertIn("unresolved_transition_clip", codes(validate_timeline(timeline)))


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
