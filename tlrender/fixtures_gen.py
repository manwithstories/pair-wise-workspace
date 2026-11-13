"""Deterministic sample-timeline generator.

Builds both timeline formats plus expected counts/interval assertions. All
numbers are generated from the same rational timebase used by the parser;
no external files are referenced.
"""
from __future__ import annotations

import json
from pathlib import Path

from .timebase import TimeBase

FPS_TEXT = "24000/1001"
MIN_READABLE_MS = 400


def _f(ms: int, tb: TimeBase) -> int:
    return tb.ms_to_frame(ms)


def _cue_line(cid: str, start_ms: int, end_ms: int, text: str, pri: int | None,
              tb: TimeBase) -> str:
    sf, ef = _f(start_ms, tb), _f(end_ms, tb)
    pri_part = f"pri {pri} " if pri is not None else ""
    return f'cue {cid} {sf} {ef} {pri_part}"{text}"'


def build_v2() -> str:
    tb = TimeBase.parse(FPS_TEXT)
    lines = ["@timeline v2", f"fps {FPS_TEXT}", ""]

    def trim_group(track: int, gid: int, base: int):
        a = f"s{gid:02d}a"
        b = f"s{gid:02d}b"
        return [
            f"# group {gid}: two-cue trim, {b} keeps priority",
            f"cue {a} {_f(base, tb)} {_f(base + 1400, tb)} "
            f'pri 1 "subtitle {gid} first"',
            f"cue {b} {_f(base + 1000, tb)} {_f(base + 2400, tb)} "
            f'pri 5 "subtitle {gid} second"',
            "",
        ]

    lines.append("track 1")
    for gid, base in enumerate((0, 3000, 6000, 9000, 12000), start=1):
        lines.extend(trim_group(1, gid, base))
    lines.append("# group 6: touching endpoints, half-open -> no overlap")
    base = 15000
    lines.append(
        f"cue s06a {_f(base, tb)} {_f(base + 1000, tb)} "
        f'pri 1 "subtitle 6 first"'
    )
    lines.append(
        f"cue s06b {_f(base + 1000, tb)} {_f(base + 2000, tb)} "
        f'pri 5 "subtitle 6 second"'
    )
    lines.append("")
    lines.append(_cue_line("f01", 18400, 19000, "filler one", None, tb))
    lines.append("")

    lines.append("track 2")
    for gid, base in enumerate((0, 3000, 6000), start=7):
        lines.extend(trim_group(2, gid, base))
    lines.append("# group 10: truncated remainder is 300 ms -> drop")
    lines.append(
        f"cue s10a {_f(9000, tb)} {_f(10200, tb)} "
        f'pri 9 "kept whole"'
    )
    lines.append(
        f"cue s10b {_f(9600, tb)} {_f(10500, tb)} "
        f'pri 2 "short remainder"'
    )
    lines.append("")
    fillers = (
        ("f02", 11000, 11600, "filler two"),
        ("f03", 12000, 12700, "filler three"),
        ("f04", 13200, 13900, "filler four"),
        ("f05", 14500, 15100, "filler five"),
        ("f06", 15800, 16500, "filler six"),
        ("f08", 18200, 18800, "filler eight"),
    )
    for cid, start, end, text in fillers:
        lines.append(_cue_line(cid, start, end, text, None, tb))
    lines.append("")

    lines.append("# transition avoidance demonstrations (track 2)")
    lines.append(
        f"transition cut {_f(28000, tb)} {_f(28000, tb)}"
    )
    lines.append(_cue_line("d01", 27200, 28800, "crosses a cut", None, tb))
    lines.append(
        f"transition dissolve {_f(29500, tb)} {_f(30500, tb)}"
    )
    lines.append(_cue_line("d02", 28800, 30800, "crosses a dissolve", None, tb))
    lines.append(
        f"transition wipe {_f(32000, tb)} {_f(33000, tb)}"
    )
    lines.append(_cue_line("d03", 31800, 33300, "crosses a wipe and shifts", None, tb))
    lines.append("@end")
    return "\n".join(lines) + "\n"


def build_v1() -> str:
    lines = ["@timeline v1", 'head "legacy pilot cut"', ""]
    shot_idx = 0
    for gid, base in enumerate((0, 3000, 6000, 9000), start=1):
        shot_idx += 1
        lines.append(f"shot shot{shot_idx:02d} {base} {base + 2800}")
        lines.append(f'cue h{gid:02d}a {base} {base + 1500} pri 2 "legacy {gid} alpha"')
        lines.append(f'cue h{gid:02d}b {base + 1200} {base + 1600} pri 8 "legacy {gid} beta"')
        lines.append(f'cue h{gid:02d}c {base + 1500} {base + 2400} pri 3 "legacy {gid} gamma"')
        lines.append("")
        cut_at = base + 2800
        lines.append(f"transition cut {cut_at} {cut_at}")
        lines.append("")
    lines.append("@end")
    return "\n".join(lines) + "\n"


def build_expected() -> dict:
    return {
        "fps": FPS_TEXT,
        "summary": {
            "total": 42,
            "resolved": 17,
            "dropped": 1,
            "frames": 42,
            "hash_mismatch": 0,
        },
        "dropped_cues": ["s10b"],
        "overlap_groups": [f"s{g:02d}{side}" for g in range(1, 11) for side in "ab"],
        "legacy_groups": [f"h{g:02d}{side}" for g in range(1, 5) for side in "abc"],
        "transition_cues": ["d01", "d02", "d03"],
        "transition_expect": {
            "d01": {"final": [27986, 28820], "moved": 1, "resolved": 0},
            "d02": {"final": [28820, 29488], "moved": 1, "resolved": 0},
            "d03": {"final": [32991, 34492], "moved": 1, "resolved": 0},
        },
        "drop_expect": {"cue": "s10b", "final_end_minus_start": 292},
    }


def generate_fixtures(directory: str | Path = "fixtures") -> list[Path]:
    out = Path(directory)
    out.mkdir(parents=True, exist_ok=True)
    paths = [
        (out / "timeline_v2.tl", build_v2()),
        (out / "timeline_v1.tl", build_v1()),
    ]
    for path, content in paths:
        path.write_text(content, encoding="utf-8")
    expected_path = out / "expected.json"
    expected_path.write_text(
        json.dumps(build_expected(), ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    return [p for p, _ in paths] + [expected_path]


if __name__ == "__main__":
    for generated in generate_fixtures():
        print(generated)
