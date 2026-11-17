"""Command-line interface: demo / generate / track / stabilize.

Exit codes: 0 ok, 2 parameter or input error, 3 insufficient tracked
points, 4 numerical instability or export failure.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

import numpy as np

from . import (EXIT_OK, EXIT_NUMERICAL, InputError, InsufficientTracksError,
               NumericalError, StabilizerError)
from . import frameio, metrics as metrics_mod
from . import motion as motion_mod
from .motion import (band_translations, compose_path, pose_params,
                     params_pose, robust_affine, robust_euclidean,
                     sanitize_transforms)
from .rolling_shutter import (band_slopes, estimate_parameters,
                              model_row_offsets, correct_points)
from .smoother import smooth_path
from .stabilize import stabilize_frames, valid_region
from .synthetic import generate_sequence
from .tracker import FeatureTracker

MIN_ACTIVE_POINTS = 20


def run_pipeline(frames: np.ndarray, min_points: int = MIN_ACTIVE_POINTS,
                 rs_assumed_direction: int = 1) -> dict:
    """Track -> estimate -> RS-correct -> smooth. Returns all artifacts."""
    n_frames = len(frames)
    if n_frames < 3:
        raise InputError("need at least 3 frames", {"frames": n_frames})
    result = FeatureTracker().run(frames)
    counts = [result.active_count(k) for k in range(result.n_frames)]
    if not counts or float(np.mean(counts)) < min_points:
        raise InsufficientTracksError(
            "not enough valid tracked points",
            {"mean_active": float(np.mean(counts)) if counts else 0.0,
             "required": min_points})

    n_rows = frames.shape[1]
    ident = np.array([[1.0, 0.0, 0.0], [0.0, 1.0, 0.0]])
    # pass 1: per-pair euclidean fits for RS slope/velocity estimation
    cache: dict[int, tuple] = {}
    slopes, velocities, vel_pairs = [], [], []
    pair_failures = []
    for k in range(1, n_frames):
        src, dst, ids = result.matches(k)
        if len(ids) < 8:
            pair_failures.append({"pair": k - 1,
                                  "reason": f"matches={len(ids)}"})
            continue
        try:
            me, _, _, _ = robust_euclidean(src, dst)
        except StabilizerError:
            pair_failures.append({"pair": k - 1, "reason": "euclidean fit"})
            continue
        centers, boff, bcounts = band_translations(src, dst, me, n_bands=6,
                                                   frame_h=n_rows)
        cache[k] = (src, dst)
        slopes.append(band_slopes(centers, boff, bcounts, n_rows))
        velocities.append(me[:, 2])
        vel_pairs.append(k)
    if len(cache) < max(2, n_frames // 4):
        raise InsufficientTracksError(
            "too few usable frame pairs",
            {"usable_pairs": len(cache), "frames": n_frames})
    rs = estimate_parameters(slopes, velocities, n_rows,
                             assumed_direction=rs_assumed_direction)
    apply_rs = bool(rs["significant"])
    vel_of = dict(zip(vel_pairs, velocities))

    def jello_velocity(k):  # velocity driving frame k's readout skew:
        # frame k is exposed while the camera moves from k to k+1, so
        # its jello is driven by the velocity of pair (k, k+1)
        for j in (k + 1, k, k - 1):
            if j in vel_of:
                return vel_of[j]
        return np.zeros(2)

    def rs_correct(pts, frame_k):
        return correct_points(pts, model_row_offsets(
            jello_velocity(frame_k), rs["row_delay"], rs["direction"],
            n_rows))

    # pass 2: final fits on RS-corrected points, aligned to pairs.
    # The camera path uses the euclidean (rotation+translation) model —
    # physically exact for a drone between adjacent frames; a full
    # affine's scale/shear DOFs absorb rolling-shutter skew and corrupt
    # the translation. The affine fit still classifies inliers and
    # drives the piecewise (band) analysis above.
    transforms, fit_rms, inlier_masks = [], [], {}
    prev_t = None
    for k in range(1, n_frames):
        m = None
        if k in cache:
            src, dst = cache[k]
            if apply_rs:
                src, dst = rs_correct(src, k - 1), rs_correct(dst, k)
            try:
                m, inl, _, rms = robust_euclidean(src, dst)
                # affine residual for the track-quality metric
                _, _, _, rms_aff = robust_affine(src, dst)
                inlier_masks[k] = inl
                fit_rms.append(min(rms, rms_aff))
            except InsufficientTracksError:
                pair_failures.append({"pair": k - 1, "reason": "affine fit"})
        if m is None:  # failed pair: keep sequence alive, flag it
            m = prev_t if prev_t is not None else ident
            if not any(f["pair"] == k - 1 for f in pair_failures):
                pair_failures.append({"pair": k - 1, "reason": "no matches"})
        transforms.append(m)
        prev_t = m
    if not inlier_masks:
        raise NumericalError("all motion fits failed",
                             {"frames": n_frames})
    # fit_rms only covers successful fits; rebuild a pair-aligned list
    rms_iter = iter(fit_rms)
    rms_aligned = [next(rms_iter) if k in inlier_masks else 1e9
                   for k in range(1, n_frames)]
    transforms, sanitize_failures = sanitize_transforms(transforms,
                                                        rms_aligned)
    pair_failures.extend(sanitize_failures)
    raw_poses = compose_path(transforms)
    bad_pairs = [f["pair"] for f in pair_failures]
    raw_poses = motion_mod.interpolate_path(raw_poses, bad_pairs)
    smoothed, smooth_info = smooth_path(raw_poses, frames.shape[1:3])

    jello_vel = [jello_velocity(k) for k in range(n_frames)]
    row_offsets = [model_row_offsets(jello_vel[k], rs["row_delay"],
                                     rs["direction"], n_rows)
                   if apply_rs else None for k in range(n_frames)]
    rejected = metrics_mod.outlier_tracks(result, inlier_masks)
    rs_info = dict(rs)
    rs_info["applied"] = apply_rs
    return {"track_result": result, "raw_poses": raw_poses,
            "smoothed_poses": smoothed, "smooth_info": smooth_info,
            "rs_info": rs_info, "row_offsets": row_offsets,
            "fit_residuals": fit_rms, "rejected": rejected,
            "pair_failures": pair_failures, "jello_vel": jello_vel,
            "counts": counts}


def trajectory_json(pipe: dict, frame_shape) -> dict:
    tr = pipe["track_result"]
    feats = [{"frame": k, "ids": tr.ids[k].tolist(),
              "xy": np.round(tr.xy[k], 3).tolist()}
             for k in range(tr.n_frames)]
    return {
        "schema": "stabilizer-trajectory/1",
        "frame_shape": list(frame_shape),
        "n_frames": tr.n_frames,
        "features": feats,
        "tracks": {str(t): v for t, v in tr.tracks.items()},
        "camera_path": [list(pose_params(p)) for p in pipe["raw_poses"]],
        "smoothed_path": [list(pose_params(p)) for p in pipe["smoothed_poses"]],
        "jello_velocity": [np.round(v, 4).tolist() for v in pipe["jello_vel"]],
        "rolling_shutter": pipe["rs_info"],
        "smooth_info": pipe["smooth_info"],
    }


def _metrics_and_print(pipe, n_frames):
    m = metrics_mod.compute_metrics(
        pipe["track_result"], pipe["raw_poses"], pipe["smoothed_poses"],
        pipe["smooth_info"], pipe["rs_info"], pipe["fit_residuals"],
        pipe["rejected"], pipe["pair_failures"], n_frames,
        MIN_ACTIVE_POINTS)
    keys = ["frames", "tracked_points", "track_residual_px",
            "jitter_before_px", "jitter_after_px", "rejected_tracks"]
    line = " ".join(f"{k}={m[k]:.2f}" if isinstance(m[k], float)
                    else f"{k}={m[k]}" for k in keys)
    line += (f" crop_ratio={m['crop_ratio']:.3f}"
             f" rolling_shutter_corrected="
             f"{'true' if m['rolling_shutter_corrected'] else 'false'}")
    print(line)
    return m


def cmd_demo(args) -> int:
    out = Path(args.output)
    out.mkdir(parents=True, exist_ok=True)
    frames, truth = generate_sequence(
        args.frames, args.width, args.height, args.seed, pan=args.pan,
        rolling_shutter=args.rolling_shutter, jitter_px=args.jitter)
    pipe = run_pipeline(frames)
    stab, valid_all, stab_info = stabilize_frames(
        frames, pipe["raw_poses"], pipe["smoothed_poses"],
        pipe["row_offsets"])
    frameio.save_frames(stab, out / "stabilized")
    traj = trajectory_json(pipe, frames.shape[1:3])
    traj["valid_region"] = valid_region(valid_all)
    traj["truth"] = truth
    frameio.save_json(traj, out / "trajectories.json")
    m = _metrics_and_print(pipe, len(frames))
    m["valid_region"] = traj["valid_region"]
    m["valid_fraction"] = stab_info["valid_fraction"]
    frameio.save_json(m, out / "metrics.json")
    print(f"outputs: {out}/stabilized ({len(frames)} frames), "
          f"{out}/trajectories.json, {out}/metrics.json")
    return EXIT_OK


def cmd_generate(args) -> int:
    out = Path(args.output)
    frames, truth = generate_sequence(
        args.frames, args.width, args.height, args.seed, pan=args.pan,
        rolling_shutter=args.rolling_shutter, jitter_px=args.jitter)
    frameio.save_frames(frames, out)
    frameio.save_json(truth, out / "truth.json")
    print(f"wrote {len(frames)} frames to {out}")
    return EXIT_OK


def cmd_track(args) -> int:
    frames = frameio.load_frames(args.frames_dir)
    pipe = run_pipeline(frames)
    traj = trajectory_json(pipe, frames.shape[1:3])
    frameio.save_json(traj, args.output)
    _metrics_and_print(pipe, len(frames))
    print(f"trajectory: {args.output}")
    return EXIT_OK


def cmd_stabilize(args) -> int:
    frames = frameio.load_frames(args.frames_dir)
    traj = frameio.load_json(args.trajectory)
    if traj.get("schema") != "stabilizer-trajectory/1":
        raise InputError("not a stabilizer trajectory file",
                         {"path": args.trajectory})
    # reuse the stored trajectory + smoothing; no re-estimation here
    raw_poses = [params_pose(*p) for p in traj["camera_path"]]
    smoothed = [params_pose(*p) for p in traj["smoothed_path"]]
    rs = traj["rolling_shutter"]
    n_rows = frames.shape[1]
    vel = traj["jello_velocity"]
    row_offsets = [model_row_offsets(vel[min(k, len(vel) - 1)],
                                     rs["row_delay"], rs["direction"], n_rows)
                   if rs.get("applied") else None
                   for k in range(len(frames))]
    stab, valid_all, info = stabilize_frames(frames, raw_poses, smoothed,
                                             row_offsets)
    out = Path(args.output)
    frameio.save_frames(stab, out / "stabilized")
    report = {"valid_region": valid_region(valid_all),
              "valid_fraction": info["valid_fraction"],
              "rolling_shutter_corrected": bool(rs.get("applied"))}
    frameio.save_json(report, out / "stabilize_report.json")
    print(f"stabilized {len(frames)} frames -> {out}/stabilized "
          f"(valid_fraction={info['valid_fraction']:.3f})")
    return EXIT_OK


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="stabilizer",
                                description="pure-local video stabilization")
    sub = p.add_subparsers(dest="command", required=True)

    def add_gen(sp):
        sp.add_argument("--frames", type=int, default=180)
        sp.add_argument("--width", type=int, default=320)
        sp.add_argument("--height", type=int, default=180)
        sp.add_argument("--seed", type=int, default=17)
        sp.add_argument("--pan", type=float, default=12.0)
        sp.add_argument("--rolling-shutter", type=float, default=0.0)
        sp.add_argument("--jitter", type=float, default=10.0)
        sp.add_argument("--output", required=True)

    add_gen(sub.add_parser("demo", help="synthetic end-to-end demo"))
    add_gen(sub.add_parser("generate", help="write synthetic frames"))
    sp = sub.add_parser("track", help="frames -> trajectory JSON")
    sp.add_argument("--frames-dir", required=True)
    sp.add_argument("--output", required=True)
    sp = sub.add_parser("stabilize", help="frames + trajectory -> frames")
    sp.add_argument("--frames-dir", required=True)
    sp.add_argument("--trajectory", required=True)
    sp.add_argument("--output", required=True)
    return p


def main(argv=None) -> int:
    args = build_parser().parse_args(argv)
    handlers = {"demo": cmd_demo, "generate": cmd_generate,
                "track": cmd_track, "stabilize": cmd_stabilize}
    try:
        return handlers[args.command](args)
    except StabilizerError as exc:
        print(f"error[{exc.exit_code}]: {exc}", file=sys.stderr)
        if exc.diagnostics:
            print(f"diagnostics: {exc.diagnostics}", file=sys.stderr)
        return exc.exit_code
    except (OSError, ValueError) as exc:
        print(f"error[{EXIT_NUMERICAL}]: {exc}", file=sys.stderr)
        return EXIT_NUMERICAL


if __name__ == "__main__":
    sys.exit(main())
