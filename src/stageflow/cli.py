"""stageflow command line entry point.

Exit codes:
  0  delivery rendered and verified
  2  parameter or manifest error
  3  media corruption / timeline inconsistency
  4  interrupted and not yet resumed
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

from .errors import StageflowError
from .manifest import build_manifest, verify_outputs, write_manifest
from .pipeline import PipelineConfig, run_pipeline
from .timeline import load_timeline


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="stageflow", description=__doc__)
    sub = p.add_subparsers(dest="command", required=True)
    r = sub.add_parser("render", help="render a delivery from a timeline manifest")
    r.add_argument("--manifest", required=True, help="timeline manifest JSON")
    r.add_argument("--out", required=True, help="output directory")
    r.add_argument("--target-bitrate", type=int, default=4_000_000)
    r.add_argument("--queue-blocks", type=int, default=3,
                   help="bounded queue capacity in blocks")
    r.add_argument("--block-frames", type=int, default=4096,
                   help="delivery-rate frames per processing block")
    r.add_argument("--consumer-delay-ms", type=float, default=0.0,
                   help="simulate a slow consumer (per-block delay)")
    r.add_argument("--stall-threshold-ms", type=float, default=500.0)
    r.add_argument("--on-corrupt", choices=["fail", "mute"], default="fail")
    r.add_argument("--resume", action="store_true",
                   help="resume from the last completed block")
    r.add_argument("--cancel-after-blocks", type=int, default=None,
                   help="cancel after N new blocks (test/demo hook)")
    return p


def cmd_render(args: argparse.Namespace) -> int:
    if args.target_bitrate <= 0:
        raise StageflowError(
            "PARAM_BITRATE", "--target-bitrate must be positive", stage="cli"
        )
    if args.queue_blocks < 1 or args.block_frames < 1:
        raise StageflowError(
            "PARAM_QUEUE", "--queue-blocks and --block-frames must be >= 1", stage="cli"
        )
    timeline = load_timeline(args.manifest)
    cfg = PipelineConfig(
        out_dir=Path(args.out),
        target_bitrate=args.target_bitrate,
        queue_blocks=args.queue_blocks,
        block_frames=args.block_frames,
        consumer_delay_ms=args.consumer_delay_ms,
        stall_threshold_ms=args.stall_threshold_ms,
        on_corrupt=args.on_corrupt,
        resume=args.resume,
        cancel_after_blocks=args.cancel_after_blocks,
    )
    result = run_pipeline(timeline, cfg)
    manifest = build_manifest(timeline, result, cfg.out_dir, cfg.target_bitrate)
    verify_outputs(timeline, manifest, cfg.out_dir)
    path = write_manifest(manifest, cfg.out_dir, timeline.job)

    video = manifest["video"]
    bitrate = manifest["bitrate"]
    audio = manifest["audio"]
    print(f"frames={video['frames']}")
    print(f"target_bitrate={bitrate['target']}")
    print(f"actual_bitrate={bitrate['actual']}")
    print(f"bitrate_error_pct={bitrate['error_pct']}")
    print(f"rms_dbfs={audio['rms_dbfs']}")
    print(f"clipping_pct={audio['clipping_pct']}")
    print(f"manifest_alignment_ms={manifest['alignment_error_ms']}")
    print(f"backpressure_wait_ms={manifest['backpressure_wait_ms']}")
    print(f"missing_blocks={manifest['missing_blocks']}")
    print(f"manifest={path.name}")
    return 0


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        if args.command == "render":
            return cmd_render(args)
        return 2
    except StageflowError as e:
        for key, value in e.locate_fields().items():
            print(f"{key}={value}", file=sys.stderr)
        return e.exit_code
    except KeyboardInterrupt:
        print("error_code=INTERRUPTED", file=sys.stderr)
        return 4


if __name__ == "__main__":
    sys.exit(main())
