"""aoa command line: demo / encode / decode / verify.

Exit codes: 0 ok; 2 invalid parameters or input; 3 numerical stability or
normalization failure; 4 render metrics out of bounds.
"""
from __future__ import annotations

import argparse
import json
import math
import sys
import wave
from pathlib import Path

import numpy as np

from aoa import decoder, encoder, fixtures, harmonics, metrics, normalization, render_paths
from aoa.types import AudioFrame, Direction, NormMode, NumericalError

EXIT_OK = 0
EXIT_USAGE = 2
EXIT_NUMERICAL = 3
EXIT_METRICS = 4

ROUNDTRIP_LIMIT = 1e-6
ENERGY_ERROR_LIMIT_DB = 1.0
CROSSTALK_LIMIT_DB = -18.0  # max_crosstalk_db must be at or below this


def read_wav(path: str) -> AudioFrame:
    """Read a PCM WAV file (16-bit or 32-bit integer) as float64."""
    p = Path(path)
    if not p.is_file():
        raise ValueError(f"input file not found: {p}")
    with wave.open(str(p), "rb") as fh:
        channels, width, rate = fh.getnchannels(), fh.getsampwidth(), fh.getframerate()
        raw = fh.readframes(fh.getnframes())
    if width == 2:
        data = fixtures.from_pcm16(np.frombuffer(raw, dtype="<i2"))
    elif width == 4:
        data = np.frombuffer(raw, dtype="<i4").astype(np.float64) / 2147483648.0
    else:
        raise ValueError(f"unsupported sample width: {width} bytes")
    return AudioFrame(data.reshape(-1, channels), rate)


def write_wav(path: str, frame: AudioFrame) -> None:
    """Write 16-bit PCM WAV after empty/NaN/Inf/clipping checks."""
    fixtures.validate_for_write(frame)
    with wave.open(str(path), "wb") as fh:
        fh.setnchannels(frame.channel_count)
        fh.setsampwidth(2)
        fh.setframerate(frame.sample_rate)
        fh.writeframes(fixtures.to_pcm16(frame.samples).tobytes())


def _json_ready(obj):
    """Recursively convert numpy types; raise on non-finite floats."""
    if isinstance(obj, dict):
        return {k: _json_ready(v) for k, v in obj.items()}
    if isinstance(obj, (list, tuple)):
        return [_json_ready(v) for v in obj]
    if isinstance(obj, np.ndarray):
        return _json_ready(obj.tolist())
    if isinstance(obj, (bool, np.bool_)):
        return bool(obj)
    if isinstance(obj, (int, np.integer)):
        return int(obj)
    if isinstance(obj, (float, np.floating)):
        value = float(obj)
        if not math.isfinite(value):
            raise NumericalError(f"non-finite value in report: {obj!r}")
        return value
    return obj


def _emit(report: dict, as_json: bool) -> None:
    if as_json:
        print(json.dumps(_json_ready(report), indent=2, sort_keys=True))
    else:
        for key, value in _json_ready(report).items():
            if isinstance(value, list) and value and isinstance(value[0], float):
                value = "[" + ", ".join(f"{v:.6g}" for v in value) + "]"
            print(f"{key}: {value}")


def run_verify(args: argparse.Namespace) -> int:
    """Run both render paths over seeded fixtures and gate the metrics."""
    if args.frames <= 0:
        raise ValueError("--frames must be positive")
    if args.sources <= 0:
        raise ValueError("--sources must be positive")
    norm_in = NormMode.parse(args.norm_in)
    norm_out = NormMode.parse(args.norm_out)
    directions = fixtures.make_directions(args.seed, args.sources)
    signals = fixtures.make_source_signals(
        args.seed, args.sources, args.frames, args.sample_rate,
        inject_silence=args.inject_silence, inject_clipping=args.inject_clipping,
    )
    layout = fixtures.make_layout(args.layout)
    enc = encoder.Encoder(norm_in, args.sample_rate)
    mixed, enc_diag = enc.encode_mix(list(zip(directions, signals)))
    converted = normalization.convert(mixed.samples, norm_in, norm_out)
    roundtrip = normalization.roundtrip_error(mixed.samples, norm_in, norm_out)
    design = decoder.build_decoder(
        layout, norm_out, strategy=args.strategy, regularization=args.regularization
    )
    warnings = list(enc_diag.warnings) + list(design.warnings)

    per_source = [
        normalization.convert(enc.encode(d, s), norm_in, norm_out)
        for d, s in zip(directions, signals)
    ]
    e_in = [metrics.total_energy(p) for p in per_source]
    e_out = [metrics.total_energy(p @ design.matrix.T) for p in per_source]
    energy_errors = [
        abs(10.0 * math.log10(o / i)) if i > 0 else 0.0 for i, o in zip(e_in, e_out)
    ]

    speaker = render_paths.render_speakers(converted, design.matrix)
    binaural = render_paths.render_binaural(speaker.signals, layout)
    basis = harmonics.basis_matrix(layout.directions, norm_out)
    gl, gr = render_paths.hrtf_ear_gains(layout)
    max_crosstalk = metrics.binaural_crosstalk_db(gl, gr)
    leakages = [
        metrics.interaural_leakage_db(p @ design.matrix.T @ gl, p @ design.matrix.T @ gr, d.azimuth_deg)
        for p, d in zip(per_source, directions)
    ]

    failures: list[dict] = []
    numerical = False
    if roundtrip > ROUNDTRIP_LIMIT:
        numerical = True
        failures.append({"gate": "roundtrip_error", "value": roundtrip, "limit": ROUNDTRIP_LIMIT})
    if not math.isfinite(design.condition_number) or design.condition_number > decoder.COND_FAILURE:
        numerical = True
        failures.append({"gate": "condition_number", "value": design.condition_number, "limit": decoder.COND_FAILURE})
    energy_error = max(energy_errors)
    if energy_error > ENERGY_ERROR_LIMIT_DB:
        worst = int(np.argmax(energy_errors))
        failures.append({"gate": "energy_error_db", "value": energy_error, "limit": ENERGY_ERROR_LIMIT_DB,
                         "direction": vars(directions[worst])})
    if max_crosstalk > CROSSTALK_LIMIT_DB:
        failures.append({"gate": "max_crosstalk_db", "value": max_crosstalk, "limit": CROSSTALK_LIMIT_DB})

    report = {
        "source_count": args.sources,
        "frame_count": args.frames,
        "sample_rate": args.sample_rate,
        "norm_in": norm_in.value,
        "norm_out": norm_out.value,
        "encoded_channels": 4,
        "speaker_channels": layout.channel_count,
        "strategy": design.strategy,
        "condition_number": design.condition_number,
        "energy_by_direction": e_in,
        "decoded_rms": speaker.rms,
        "binaural_rms": binaural.rms,
        "decoded_peak": speaker.peak,
        "max_crosstalk_db": max_crosstalk,
        "speaker_adjacent_crosstalk_db": metrics.adjacent_crosstalk_db(design.matrix, basis),
        "binaural_leakage_db": leakages,
        "energy_error_db": energy_error,
        "energy_error_by_direction_db": energy_errors,
        "roundtrip_error": roundtrip,
        "max_channel_diff_db": metrics.channel_spread_db(speaker.rms),
        "clipped_samples": enc_diag.clipped_samples,
        "warnings": warnings,
        "failures": failures,
    }
    _emit(report, args.json)
    if numerical:
        return EXIT_NUMERICAL
    return EXIT_METRICS if failures else EXIT_OK


def run_demo(args: argparse.Namespace) -> int:
    """Small human-readable end-to-end run (fixed seed, short block)."""
    ns = argparse.Namespace(
        frames=4800, seed=7, sources=6, sample_rate=48000,
        norm_in="fuma", norm_out="acn-sn3d", layout="ring8",
        strategy=decoder.ENERGY_PRESERVING, regularization=1e-6,
        inject_silence=False, inject_clipping=False, json=False,
    )
    return run_verify(ns)


def run_encode(args: argparse.Namespace) -> int:
    """Encode a mono WAV file to a 4-channel B-format WAV file."""
    direction = Direction(args.azimuth, args.elevation)
    frame = read_wav(args.input)
    if frame.channel_count != 1:
        raise ValueError(f"expected a mono input, got {frame.channel_count} channels")
    enc = encoder.Encoder(NormMode.parse(args.norm), frame.sample_rate)
    out, diag = enc.encode_mix([(direction, frame.samples[:, 0])])
    warnings = list(diag.warnings)
    if diag.clipped_samples and args.normalize:
        out = AudioFrame(out.samples / diag.peak, out.sample_rate)
        warnings.append(f"normalized by {diag.peak:.3f} to avoid clipping")
    write_wav(args.output, out)
    _emit({"output": args.output, "channels": 4, "frames": out.frame_count,
           "peak": metrics.peak(out.samples), "warnings": warnings}, args.json)
    return EXIT_OK


def run_decode(args: argparse.Namespace) -> int:
    """Decode a 4-channel B-format WAV file to loudspeaker feeds."""
    frame = read_wav(args.input)
    if frame.channel_count != 4:
        raise ValueError(f"expected 4 B-format channels, got {frame.channel_count}")
    if args.sample_rate and frame.sample_rate != args.sample_rate:
        raise ValueError(
            f"sample rate mismatch: file is {frame.sample_rate}, "
            f"expected {args.sample_rate}"
        )
    layout = fixtures.make_layout(args.layout)
    design = decoder.build_decoder(
        layout, NormMode.parse(args.norm),
        strategy=args.strategy, regularization=args.regularization,
    )
    rendered = render_paths.render_speakers(frame.samples, design.matrix)
    warnings = list(design.warnings)
    signals = rendered.signals
    if rendered.peak > 1.0 and args.normalize:
        signals = signals / rendered.peak
        warnings.append(f"normalized by {rendered.peak:.3f} to avoid clipping")
    write_wav(args.output, AudioFrame(signals, frame.sample_rate))
    _emit({"output": args.output, "channels": layout.channel_count,
           "frames": frame.frame_count, "rms": metrics.rms_per_channel(signals),
           "warnings": warnings}, args.json)
    return EXIT_OK


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="aoa", description=__doc__)
    sub = p.add_subparsers(dest="command", required=True)

    v = sub.add_parser("verify", help="run the full two-path verification")
    v.add_argument("--frames", type=int, default=48000)
    v.add_argument("--seed", type=int, required=True, help="explicit RNG seed (required)")
    v.add_argument("--sources", type=int, default=6)
    v.add_argument("--sample-rate", type=int, default=48000)
    v.add_argument("--norm-in", required=True, help="acn-sn3d | acn-n3d | fuma")
    v.add_argument("--norm-out", required=True, help="acn-sn3d | acn-n3d | fuma")
    v.add_argument("--layout", default="ring8", help=f"one of {fixtures.layout_names()}")
    v.add_argument("--strategy", default=decoder.ENERGY_PRESERVING, choices=decoder.STRATEGIES)
    v.add_argument("--regularization", type=float, default=1e-6)
    v.add_argument("--inject-silence", action="store_true")
    v.add_argument("--inject-clipping", action="store_true")
    v.add_argument("--json", action="store_true")
    v.set_defaults(func=run_verify)

    d = sub.add_parser("demo", help="short human-readable end-to-end demo")
    d.set_defaults(func=run_demo)

    e = sub.add_parser("encode", help="encode a mono WAV to B-format WAV")
    e.add_argument("--azimuth", type=float, required=True)
    e.add_argument("--elevation", type=float, default=0.0)
    e.add_argument("--input", required=True)
    e.add_argument("--output", required=True)
    e.add_argument("--norm", default="acn-sn3d")
    e.add_argument("--normalize", action="store_true")
    e.add_argument("--json", action="store_true")
    e.set_defaults(func=run_encode)

    c = sub.add_parser("decode", help="decode a B-format WAV to loudspeaker feeds")
    c.add_argument("--input", required=True)
    c.add_argument("--output", required=True)
    c.add_argument("--layout", default="ring8")
    c.add_argument("--norm", default="acn-sn3d")
    c.add_argument("--strategy", default=decoder.ENERGY_PRESERVING, choices=decoder.STRATEGIES)
    c.add_argument("--regularization", type=float, default=1e-6)
    c.add_argument("--sample-rate", type=int, default=0)
    c.add_argument("--normalize", action="store_true")
    c.add_argument("--json", action="store_true")
    c.set_defaults(func=run_decode)
    return p


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return int(args.func(args))
    except (NumericalError, normalization.NormalizationError) as exc:
        print(f"numerical/normalization error: {exc}", file=sys.stderr)
        return EXIT_NUMERICAL
    except (ValueError, wave.Error, OSError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_USAGE


if __name__ == "__main__":
    sys.exit(main())
