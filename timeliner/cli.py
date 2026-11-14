"""Terminal CLI: compose, verify, probe, synthaudio. Exit codes per spec."""

import argparse
import json
import os
import resource
import sys

from . import audio as audio_mod
from .digest import (MANIFEST_NAME, build_manifest, check_manifest_consistency,
                     load_manifest, require_consistent, verify_frames,
                     write_manifest)
from .errors import AVAlignError, TimelineError, TimelinerError
from .exporter import run_export
from .model import load_timeline


def _peak_rss_mb():
    self_rss = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
    child_rss = resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss
    total = max(self_rss, child_rss)
    if sys.platform == "darwin":
        return total / (1024.0 * 1024.0)
    return total / 1024.0


def _print(obj, stream=sys.stdout):
    stream.write(json.dumps(obj, sort_keys=True) + "\n")
    stream.flush()


def cmd_compose(args):
    if args.frames <= 0:
        raise TimelineError("--frames must be a positive integer")
    if args.fps <= 0:
        raise TimelineError("--fps must be positive")
    if args.workers < 1:
        raise TimelineError("--workers must be >= 1")
    timeline = load_timeline(args.timeline)
    mix = audio_mod.mix_tracks(timeline.audio, args.assets,
                               timeline.duration)
    rms = audio_mod.frame_rms(mix, args.fps, args.frames)
    av_max, av_mean = audio_mod.alignment_errors(timeline.audio, args.fps)
    if av_max > args.max_av_err_ms + 1e-6:
        raise AVAlignError(
            "audio/video alignment error %.3f ms exceeds --max-av-err-ms %.3f"
            % (av_max, args.max_av_err_ms))
    result = run_export(args.timeline, args.assets, args.out, args.fps,
                        args.frames, args.workers, resume=args.resume,
                        no_frames=args.no_frames)
    manifest = build_manifest(timeline, args.fps, args.frames,
                              result["digests"], av_max, av_mean,
                              result["blend_calls"])
    write_manifest(os.path.join(args.out, MANIFEST_NAME), manifest)
    _print({
        "frames_written": 0 if args.no_frames else args.frames,
        "digest_root": manifest["merkle_root"],
        "av_max_err_ms": round(av_max, 3),
        "av_mean_err_ms": round(av_mean, 3),
        "rms_first_frame": round(rms[0], 6) if rms else 0.0,
        "blend_calls": result["blend_calls"],
        "resumed_from": result["resumed_from"],
        "peak_rss_mb": round(_peak_rss_mb(), 1),
        "manifest": os.path.join(args.out, MANIFEST_NAME),
    })
    return 0


def cmd_verify(args):
    manifest = load_manifest(args.manifest)
    problems = check_manifest_consistency(manifest)
    n = manifest["frames"]
    frames_dir = args.frames
    if frames_dir and os.path.isdir(frames_dir):
        matched, mismatches = verify_frames(manifest, frames_dir)
        problems.extend("frame %d: %s" % (i, why) for i, why in mismatches)
        _print({"frames": n, "matched": matched,
                "mismatch": n - matched, "mode": "frames"})
    else:
        matched = 0 if problems else n
        _print({"frames": n, "matched": matched,
                "mismatch": 0 if not problems else n,
                "mode": "manifest-only"})
    require_consistent(problems)
    return 0


def cmd_probe(args):
    timeline = load_timeline(args.timeline)
    layers = []
    for layer in timeline.layers:
        path = os.path.join(args.assets, layer.asset)
        layers.append({
            "id": layer.id, "asset": layer.asset,
            "interval": [layer.start, layer.end], "blend": layer.blend,
            "keyframes": len(layer.keyframes),
            "asset_exists": os.path.isfile(path),
        })
    clips = [{"asset": c.asset, "start": c.start, "gain": c.gain,
              "asset_exists": os.path.isfile(os.path.join(args.assets, c.asset))}
             for c in timeline.audio]
    _print({
        "canvas": [timeline.width, timeline.height],
        "duration": timeline.duration,
        "layers": layers,
        "audio": clips,
        "assets_ok": all(l["asset_exists"] for l in layers)
        and all(c["asset_exists"] for c in clips),
    })
    return 0


def cmd_synthaudio(args):
    audio_mod.synth_wav(args.out, args.kind, args.duration, seed=args.seed)
    _print({"path": args.out, "kind": args.kind, "duration": args.duration,
            "sample_rate": audio_mod.SAMPLE_RATE})
    return 0


def build_parser():
    parser = argparse.ArgumentParser(
        prog="timeliner", description="Offline timeline compositor")
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("compose", help="render frames and write a manifest")
    p.add_argument("--timeline", required=True)
    p.add_argument("--assets", required=True)
    p.add_argument("--out", required=True)
    p.add_argument("--fps", type=float, default=24.0)
    p.add_argument("--frames", type=int, required=True)
    p.add_argument("--workers", type=int, default=1)
    p.add_argument("--resume", action="store_true")
    p.add_argument("--no-frames", action="store_true")
    p.add_argument("--max-av-err-ms", type=float, default=1.0)
    p.set_defaults(func=cmd_compose)

    p = sub.add_parser("verify", help="recompute digests and compare")
    p.add_argument("--manifest", required=True)
    p.add_argument("--frames", default=None)
    p.set_defaults(func=cmd_verify)

    p = sub.add_parser("probe", help="inspect a timeline and its assets")
    p.add_argument("--timeline", required=True)
    p.add_argument("--assets", required=True)
    p.set_defaults(func=cmd_probe)

    p = sub.add_parser("synthaudio", help="synthesize a deterministic WAV")
    p.add_argument("--out", required=True)
    p.add_argument("--kind", choices=["voice", "music"], required=True)
    p.add_argument("--duration", type=float, required=True)
    p.add_argument("--seed", type=int, default=0)
    p.set_defaults(func=cmd_synthaudio)
    return parser


def main(argv=None):
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except TimelinerError as exc:
        _print({"error": str(exc), "exit_code": exc.exit_code},
               stream=sys.stderr)
        return exc.exit_code


if __name__ == "__main__":
    sys.exit(main())
