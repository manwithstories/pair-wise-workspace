"""Deterministic fixture generation entry point for the test suite.

Mirrors the CLI ``demo`` scenarios but returns paths directly so unit
tests never shell out.  All data is synthesized locally; no external
assets are referenced.
"""

from __future__ import annotations

from pathlib import Path

from relay_timeline.fixtures import FixtureSpec, build_fixture


def generate_suite(root: Path) -> dict[str, Path]:
    """Create the success, boundary and damage fixtures under ``root``."""
    root = Path(root)
    root.mkdir(parents=True, exist_ok=True)
    scenarios = {
        "clean": FixtureSpec(out_dir=root / "clean", fmt="hls",
                             duration=6.0, vfr=True,
                             wrap_at=8589934500),
        "wrap": FixtureSpec(out_dir=root / "wrap", fmt="dash",
                            duration=6.0, wrap_at=8589934500),
        "drift": FixtureSpec(out_dir=root / "drift", fmt="hls",
                             duration=6.0, drift_ppm=250.0),
        "truncated": FixtureSpec(out_dir=root / "truncated", fmt="hls",
                                 duration=6.0, fault="truncated"),
        "zero": FixtureSpec(out_dir=root / "zero", fmt="hls",
                            duration=6.0, fault="zero"),
        "missing": FixtureSpec(out_dir=root / "missing", fmt="dash",
                               duration=6.0, fault="missing"),
        "out_of_order": FixtureSpec(out_dir=root / "out_of_order",
                                    fmt="hls", duration=6.0),
    }
    manifests: dict[str, Path] = {}
    for name, spec in scenarios.items():
        result = build_fixture(spec)
        manifests[name] = result.manifest
    return manifests


if __name__ == "__main__":  # pragma: no cover
    import sys
    target = Path(sys.argv[1] if len(sys.argv) > 1 else "generated_fixtures")
    generate_suite(target)
    print(f"fixtures written under {target}")
