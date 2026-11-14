"""audiocut command line interface: detect / export / report / selfcheck.

Exit codes: 0 success, 2 argument error (argparse), 3 input or WAV
structure error, 4 no segments detected, 5 database or export failure.
"""
from __future__ import annotations

import argparse
import json
import logging
import os
import sqlite3
import sys

from .export.slicer import ExportError, export_run
from .report.summary import build_report, report_json
from .store import db as store
from .wavio import reader

EXIT_OK = 0
EXIT_ARGS = 2
EXIT_INPUT = 3
EXIT_NO_SEGMENTS = 4
EXIT_IO = 5

log = logging.getLogger("audiocut")


class NoSegmentsError(Exception):
    """Detection finished but found no usable speech segment."""


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="audiocut",
        description="Split museum guide recordings into exhibit segments.")
    sub = parser.add_subparsers(dest="cmd", required=True)
    det = sub.add_parser("detect", help="detect segments and store a run")
    det.add_argument("-i", "--input", required=True, help="source WAV file")
    det.add_argument("-d", "--db", required=True, help="SQLite database path")
    det.add_argument("--min-silence-ms", type=float, default=320.0)
    det.add_argument("--min-speech-ms", type=float, default=250.0)
    det.add_argument("--frame-ms", type=float, default=25.0)
    det.add_argument("--hop-ms", type=float, default=10.0)
    det.add_argument("--pad-ms", type=float, default=60.0)
    det.add_argument("--enter-offset-db", type=float, default=10.0)
    det.add_argument("--exit-offset-db", type=float, default=6.0)
    exp = sub.add_parser("export", help="slice source WAV per stored run")
    exp.add_argument("-d", "--db", required=True, help="SQLite database path")
    exp.add_argument("-o", "--out", required=True, help="output directory")
    rep = sub.add_parser("report", help="summarise the latest run")
    rep.add_argument("-d", "--db", required=True, help="SQLite database path")
    rep.add_argument("--json", action="store_true", help="emit JSON")
    chk = sub.add_parser("selfcheck", help="end-to-end sanity checks")
    chk.add_argument("--workdir", default="work/selfcheck",
                     help="scratch directory for generated fixtures")
    return parser


def _detect_params(args) -> dict:
    return {
        "min_silence_ms": args.min_silence_ms,
        "min_speech_ms": args.min_speech_ms,
        "frame_ms": args.frame_ms,
        "hop_ms": args.hop_ms,
        "pad_ms": args.pad_ms,
        "enter_offset_db": args.enter_offset_db,
        "exit_offset_db": args.exit_offset_db,
    }


def run_detect(args) -> dict:
    from .dsp import framing, noise, segments as segmod, vad
    samples, info = reader.read_mono(args.input)
    stats = framing.frame_stats(samples, info.sample_rate,
                                args.frame_ms, args.hop_ms)
    profile = noise.estimate_noise(stats.rms_db, args.enter_offset_db,
                                   args.exit_offset_db)
    log.info("noise floor %.1f dBFS, enter %.1f dB, exit %.1f dB",
             profile.noise_floor_db, profile.enter_db, profile.exit_db)
    min_sil_frames = max(1, int(round(args.min_silence_ms / args.hop_ms)))
    min_speech_frames = max(1, int(round(args.min_speech_ms / args.hop_ms)))
    coarse = vad.detect_segments(stats.rms_db, profile.enter_db,
                                 profile.exit_db, min_speech_frames,
                                 min_sil_frames)
    pad = int(round(args.pad_ms * info.sample_rate / 1000.0))
    refined = segmod.refine_edges(stats.rms_db, coarse, profile.exit_db,
                                  stats.frame_len, stats.hop,
                                  int(samples.shape[0]), pad)
    if not refined:
        raise NoSegmentsError(f"{args.input}: no speech segments detected")
    silences = segmod.silence_intervals(refined, int(samples.shape[0]))
    sil_samples, sil_sec = segmod.silence_totals(silences, info.sample_rate)
    params = _detect_params(args)
    params["noise_floor_db"] = round(profile.noise_floor_db, 2)
    params["enter_db"] = round(profile.enter_db, 2)
    params["exit_db"] = round(profile.exit_db, 2)
    run_id = store.save_run(args.db, os.path.abspath(args.input), params,
                            info.sample_rate, int(samples.shape[0]),
                            refined, silences, sil_samples, sil_sec)
    log.info("stored run %d: %d segments, silence %d samples (%.2f s)",
             run_id, len(refined), sil_samples, sil_sec)
    return {"run_id": run_id, "segments": len(refined),
            "silence_total_samples": sil_samples,
            "silence_total_sec": sil_sec}


def _print_report_text(report: dict) -> None:
    print(f"run {report['run_id']}  input {report['input']}")
    print(f"segments: {report['segments']}")
    print(f"silence: {report['silence_total_samples']} samples "
          f"({report['silence_total_sec']} s)")
    for item in report["items"]:
        print(f"  seg {item['index']:03d}: "
              f"{item['start_sample']}..{item['end_sample']}")


def _selfcheck(args) -> int:
    import hashlib

    import numpy as np

    repo_root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    if repo_root not in sys.path:
        sys.path.insert(0, repo_root)
    from fixtures import gen

    workdir = os.path.abspath(args.workdir)
    os.makedirs(workdir, exist_ok=True)
    wav_path = os.path.join(workdir, "line_a.wav")
    db_path = os.path.join(workdir, "selfcheck.db")
    out_dir = os.path.join(workdir, "out")
    results: list[tuple[str, bool, str]] = []

    def check(name, fn):
        try:
            fn()
        except Exception as exc:  # noqa: BLE001 - report any failure
            results.append((name, False, str(exc)))
            print(f"FAIL {name}: {exc}")
        else:
            results.append((name, True, ""))
            print(f"PASS {name}")

    def t_fixture():
        gen.generate(wav_path)
        samples, info = reader.read_mono(wav_path)
        assert samples.shape[0] > 0 and info.sample_rate == gen.SR

    def t_detect():
        ns = build_parser().parse_args(
            ["detect", "-i", wav_path, "-d", db_path,
             "--min-silence-ms", "320"])
        result = run_detect(ns)
        assert 6 <= result["segments"] <= 9, result

    def t_report():
        report = build_report(db_path)
        assert report is not None
        assert 6 <= report["segments"] <= 9
        assert 8.0 <= report["silence_total_sec"] <= 20.0, report[
            "silence_total_sec"]
        assert report["silence_total_samples"] > 0
        assert all("start_sample" in it and "end_sample" in it
                   for it in report["items"])

    def t_idempotent():
        ns = build_parser().parse_args(
            ["detect", "-i", wav_path, "-d", db_path,
             "--min-silence-ms", "320"])
        run_detect(ns)
        assert store.count_runs(db_path) == 1, "duplicate run stored"

    def t_export():
        items = export_run(db_path, out_dir)
        report = build_report(db_path)
        assert len(items) == report["segments"]
        _, info = reader.read_mono(wav_path)
        for item in items:
            path = os.path.join(out_dir, item["file"])
            n_bytes = item["frames"] * info.block_align
            assert os.path.getsize(path) == 44 + n_bytes + (n_bytes & 1)

    def t_sample_exact():
        report = build_report(db_path)
        first = report["items"][0]
        source, _ = reader.read_mono(wav_path)
        name = sorted(f for f in os.listdir(out_dir)
                      if f.endswith(".wav"))[0]
        sliced, _ = reader.read_mono(os.path.join(out_dir, name))
        expect = source[first["start_sample"]:first["end_sample"]]
        assert sliced.shape[0] == expect.shape[0]
        assert np.array_equal(sliced, expect), "slice samples differ"

    def t_deterministic():
        def digest():
            blob = b""
            for name in sorted(os.listdir(out_dir)):
                with open(os.path.join(out_dir, name), "rb") as fh:
                    blob += hashlib.sha1(fh.read()).digest()
            return hashlib.sha1(blob).hexdigest()

        before = digest()
        export_run(db_path, out_dir)
        assert digest() == before, "re-export changed output bytes"

    check("fixture_generation", t_fixture)
    check("detect_segment_count", t_detect)
    check("report_silence_totals", t_report)
    check("detect_idempotent", t_idempotent)
    check("export_file_sizes", t_export)
    check("export_sample_exact", t_sample_exact)
    check("export_deterministic", t_deterministic)
    passed = sum(1 for _, ok, _ in results if ok)
    print(f"selfcheck: {passed}/{len(results)} passed")
    for name, ok, err in results:
        if not ok:
            print(f"  failed: {name}: {err}")
    return EXIT_OK if passed == len(results) else 1


def main(argv=None) -> int:
    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")
    args = build_parser().parse_args(argv)
    try:
        if args.cmd == "detect":
            print(json.dumps(run_detect(args)))
            return EXIT_OK
        if args.cmd == "report":
            if args.json:
                text = report_json(args.db)
                if text is None:
                    log.error("%s: no detection run found", args.db)
                    return EXIT_IO
                print(text)
            else:
                report = build_report(args.db)
                if report is None:
                    log.error("%s: no detection run found", args.db)
                    return EXIT_IO
                _print_report_text(report)
            return EXIT_OK
        if args.cmd == "export":
            items = export_run(args.db, args.out)
            print(json.dumps({"exported": len(items),
                              "out": os.path.abspath(args.out)}))
            return EXIT_OK
        if args.cmd == "selfcheck":
            return _selfcheck(args)
    except NoSegmentsError as exc:
        log.error("%s", exc)
        return EXIT_NO_SEGMENTS
    except reader.WavError as exc:
        log.error("%s", exc)
        return EXIT_INPUT
    except FileNotFoundError as exc:
        log.error("input not found: %s", exc.filename or exc)
        return EXIT_INPUT
    except ExportError as exc:
        log.error("%s", exc)
        return EXIT_IO
    except (sqlite3.Error, OSError) as exc:
        log.error("%s", exc)
        return EXIT_IO
    return EXIT_ARGS
