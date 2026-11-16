"""Generate the WAV fixtures used by tests and the acceptance demo.

Everything is synthesized in-process; no external file is ever read.  The set
covers the awkward cases the tool must survive:

  tone_44100_stereo.wav   44.1 kHz stereo, -24 dBFS-ish 1 kHz tone
  tone_48000_51.wav       48 kHz 5.1 with an LFE channel (must be excluded)
  tone_96000_quad.wav     96 kHz quad, high-frequency content for resampling
  mono_odd_tail.wav       44.1 kHz mono whose length is not a block multiple
  clipped_loud.wav        square-ish wave driven into full scale
  quiet_music.wav         quiet multi-tone, exercises upward gain
  silent.wav              digital silence (passes through at unity, reported)

Deliberately invalid inputs are written to a separate directory so a demo
run over the valid set exits 0:

  truncated.wav           valid PCM_16 header with a truncated data chunk
  bad_channels.wav        10-channel layout outside the supported set
  nan_samples.wav         float WAV containing NaN samples
  bad_rate.wav            sample rate below the supported range

Run:
    python -m tests.generate_fixtures --out fixtures
"""

from __future__ import annotations

import argparse
import math
from array import array
from pathlib import Path

import soundfile as sf

SAMPLE_RATE = 44100


def _tone(frequency: float, seconds: float, rate: int, amplitude: float,
          phase: float = 0.0) -> array:
    frames = int(round(seconds * rate))
    step = 2.0 * math.pi * frequency / rate
    return array("d", [amplitude * math.sin(step * index + phase)
                       for index in range(frames)])


def _mix(parts: list[tuple[float, float]], seconds: float, rate: int,
         amplitude: float) -> array:
    """Sum of sines, scaled so the peak equals ``amplitude``."""

    frames = int(round(seconds * rate))
    out = array("d", bytes(8 * frames))
    for frequency, weight in parts:
        step = 2.0 * math.pi * frequency / rate
        for index in range(frames):
            out[index] += weight * math.sin(step * index)
    peak = max((abs(value) for value in out), default=1.0) or 1.0
    scale = amplitude / peak
    for index in range(frames):
        out[index] *= scale
    return out


def write_tone(path: Path, rate: int, channels: int, seconds: float,
               amplitude: float, frequency: float = 1000.0,
               subtype: str = "PCM_24") -> Path:
    base = _tone(frequency, seconds, rate, amplitude)
    planes = [base] + [_tone(frequency * (channel + 1), seconds, rate,
                             amplitude * 0.8) for channel in range(1, channels)]
    return _write_planes(path, planes, rate, subtype)


def _write_planes(path: Path, planes: list[array], rate: int, subtype: str) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    frames = len(planes[0])
    interleaved = array("d", bytes(8 * frames * len(planes)))
    for index, plane in enumerate(planes):
        interleaved[index::len(planes)] = array("d", plane[:frames])
    with sf.SoundFile(str(path), "w", samplerate=rate, channels=len(planes),
                      subtype=subtype, format="WAV") as handle:
        handle.buffer_write(interleaved, "float64")
    return path


def make_odd_tail(path: Path, rate: int = 44100, frames: int = 12345) -> Path:
    """Mono file whose frame count is deliberately not a block multiple."""

    values = array("d", [0.3 * math.sin(2 * math.pi * 440 * i / rate)
                         for i in range(frames)])
    return _write_planes(path, [values], rate, "PCM_16")


def make_clipped(path: Path, rate: int = 44100) -> Path:
    """A sine driven past full scale, then hard-limited, so samples hit 1.0."""

    values = _tone(180.0, 2.0, rate, 1.8)
    for index, value in enumerate(values):
        if value > 1.0:
            values[index] = 1.0
        elif value < -1.0:
            values[index] = -1.0
    return _write_planes(path, [values, values], rate, "PCM_16")


def make_quiet(path: Path, rate: int = 44100) -> Path:
    values = _mix([(220.0, 1.0), (330.0, 0.6), (550.0, 0.3)], 3.0, rate, 0.02)
    return _write_planes(path, [values, values], rate, "PCM_24")


def make_silent(path: Path, rate: int = 48000) -> Path:
    frames = rate
    return _write_planes(path, [array("d", bytes(8 * frames)),
                                array("d", bytes(8 * frames))], rate, "PCM_16")


def make_truncated(path: Path, rate: int = 44100) -> Path:
    """Write a valid file, then cut the data chunk short without fixing sizes."""

    _write_planes(path, [_tone(440.0, 0.5, rate, 0.5)], rate, "PCM_16")
    raw = path.read_bytes()
    path.write_bytes(raw[: len(raw) - 3000])
    return path


def make_bad_channels(path: Path, rate: int = 48000) -> Path:
    """Ten channels: outside the documented 1-8 layout table."""

    planes = [_tone(300.0 * (index + 1), 0.5, rate, 0.1) for index in range(10)]
    return _write_planes(path, planes, rate, "PCM_16")


def make_nan(path: Path, rate: int = 48000) -> Path:
    values = _tone(440.0, 1.0, rate, 0.4)
    values[100] = float("nan")
    values[200] = float("inf")
    return _write_planes(path, [values, values], rate, "FLOAT")


def make_bad_rate(path: Path) -> Path:
    """Valid samples but an absurd rate outside 8000..192000."""

    return _write_planes(path, [_tone(440.0, 0.3, 4000, 0.5)], 4000, "PCM_16")


def generate_good(out_dir: Path) -> dict[str, Path]:
    """Valid inputs: the set the acceptance demo processes end to end."""

    out_dir = Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    made: dict[str, Path] = {}
    made["tone_44100_stereo"] = write_tone(
        out_dir / "tone_44100_stereo.wav", 44100, 2, 2.0, 0.25)
    made["tone_48000_51"] = write_tone(
        out_dir / "tone_48000_51.wav", 48000, 6, 2.0, 0.18, 500.0)
    made["tone_96000_quad"] = write_tone(
        out_dir / "tone_96000_quad.wav", 96000, 4, 1.5, 0.22, 8000.0)
    made["mono_odd_tail"] = make_odd_tail(out_dir / "mono_odd_tail.wav")
    made["clipped_loud"] = make_clipped(out_dir / "clipped_loud.wav")
    made["quiet_music"] = make_quiet(out_dir / "quiet_music.wav")
    made["silent"] = make_silent(out_dir / "silent.wav")
    return made


def generate_broken(out_dir: Path) -> dict[str, Path]:
    """Deliberately invalid inputs, kept apart so they cannot fail a demo."""

    out_dir = Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    return {
        "broken_truncated": make_truncated(out_dir / "truncated.wav"),
        "broken_bad_channels": make_bad_channels(out_dir / "bad_channels.wav"),
        "broken_nan": make_nan(out_dir / "nan_samples.wav"),
        "broken_bad_rate": make_bad_rate(out_dir / "bad_rate.wav"),
    }


def generate_all(out_dir: Path, broken_dir: Path | None = None) -> dict[str, Path]:
    """Both sets, merged; ``broken_dir`` defaults to ``<out>_broken``.

    The two directories are separate on purpose.  A batch run over the valid
    set is expected to exit 0, and mixing deliberately corrupt files into it
    would make the documented demo report a partial failure every time.
    """

    out_dir = Path(out_dir)
    broken_dir = Path(broken_dir) if broken_dir else out_dir.parent / f"{out_dir.name}_broken"
    made = generate_good(out_dir)
    made.update(generate_broken(broken_dir))
    return made


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--out", default="fixtures", type=Path,
                        help="directory for valid fixtures (default: fixtures)")
    parser.add_argument("--broken-out", type=Path, default=None,
                        help="directory for invalid fixtures "
                             "(default: <out>_broken)")
    args = parser.parse_args(argv)
    valid = generate_good(args.out)
    broken_dir = args.broken_out or Path(f"{args.out}_broken")
    broken = generate_broken(broken_dir)
    for name, path in {**valid, **broken}.items():
        print(f"{name:22s} {path}  ({path.stat().st_size} bytes)")
    print(f"\n{len(valid)} valid fixtures in {args.out}")
    print(f"{len(broken)} invalid fixtures in {broken_dir}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
