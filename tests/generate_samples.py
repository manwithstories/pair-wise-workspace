"""Fixture generation entry point used by the test suite.

Run it directly to materialise the sample corpus in a directory of your choice:

    .venv/bin/python tests/generate_samples.py /tmp/avtimeline-samples
"""

from __future__ import annotations

import json
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src"))

from avtimeline.probe import probe_media  # noqa: E402
from avtimeline.samples import generate_samples, sample_digests  # noqa: E402


def describe(directory: str) -> dict:
    samples = generate_samples(directory, force=True)
    report = {"directory": directory, "files": sample_digests(samples), "streams": {}}
    for path in samples.all_paths():
        if not os.path.exists(path):
            continue
        probe = probe_media(path)
        report["streams"][os.path.basename(path)] = {
            "duration": str(probe.media_duration) if probe.media_duration else None,
            "keyframes": [str(t) for t in probe.keyframe_times],
            "streams": [
                {
                    "kind": stream.kind,
                    "codec": stream.codec,
                    "rate": str(stream.rate) if stream.rate else None,
                    "sample_rate": stream.sample_rate,
                    "layout": stream.layout,
                }
                for stream in probe.streams
            ],
            "errors": [error.to_dict() for error in probe.errors],
        }
    return report


def main(argv: list[str]) -> int:
    directory = argv[1] if len(argv) > 1 else "samples"
    report = describe(directory)
    print(json.dumps(report, indent=2, sort_keys=True, ensure_ascii=False))
    return 0


if __name__ == "__main__":  # pragma: no cover - helper script
    raise SystemExit(main(sys.argv))
