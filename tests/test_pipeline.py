"""Pipeline tests: synthetic data, extreme parameters, failure paths."""

import json
import tempfile
import unittest
from pathlib import Path

import numpy as np

from stabilizer import (InputError, InsufficientTracksError, cli, frameio,
                        metrics as metrics_mod)
from stabilizer.features import detect_corners, track_points
from stabilizer.motion import (estimate_affine, robust_affine,
                               robust_euclidean, compose_path, pose_params,
                               sanitize_transforms)
from stabilizer.rolling_shutter import (band_slopes, correct_frame,
                                        estimate_parameters,
                                        model_row_offsets, row_frac)
from stabilizer.smoother import smooth_path, detrend
from stabilizer.synthetic import generate_sequence
from stabilizer.tracker import FeatureTracker

SMALL = dict(width=160, height=120, seed=17, pan=6)


def small_frames(n=24, **kw):
    args = dict(SMALL)
    args.update(kw)
    return generate_sequence(n, **args)


class TestSynthetic(unittest.TestCase):
    def test_reproducible(self):
        a, _ = small_frames(8)
        b, _ = small_frames(8)
        self.assertTrue(np.array_equal(a, b))

    def test_seed_changes_output(self):
        a, _ = small_frames(8)
        b, _ = small_frames(8, seed=99)
        self.assertFalse(np.array_equal(a, b))

    def test_rolling_shutter_changes_frames(self):
        a, _ = small_frames(8, rolling_shutter=0.0)
        b, _ = small_frames(8, rolling_shutter=0.5)
        self.assertFalse(np.array_equal(a, b))

    def test_occlusion_and_foreground_present(self):
        frames, truth = small_frames(30, n_occlusions=3, n_foreground=2)
        self.assertEqual(len(truth["occlusions"]), 3)
        self.assertEqual(len(truth["foreground"]), 2)


class TestFeatures(unittest.TestCase):
    def test_detect_avoids_borders(self):
        frames, _ = small_frames(4)
        pts, _ = detect_corners(frames[0])
        self.assertGreater(len(pts), 10)
        self.assertTrue((pts[:, 0] >= 12).all())
        self.assertTrue((pts[:, 1] >= 12).all())

    def test_detect_retries_on_low_texture(self):
        flat = np.full((120, 160), 128, dtype=np.uint8)
        flat[40:80, 60:100] = 200  # a single weak structure
        pts, _ = detect_corners(flat, min_points=2)
        self.assertGreaterEqual(len(pts), 1)

    def test_track_pure_shift(self):
        frames, _ = small_frames(4, rolling_shutter=0.0, jitter_px=0.0)
        pts, _ = detect_corners(frames[0])
        new, conf, resid, valid = track_points(frames[0], frames[1], pts)
        d = (new - pts)[valid]
        self.assertGreater(valid.sum(), 5)
        self.assertLess(np.median(np.abs(d[:, 0] + 6.0)), 1.0)


class TestTracker(unittest.TestCase):
    def test_lifecycle_and_redetect(self):
        frames, _ = small_frames(20, n_occlusions=2)
        tr = FeatureTracker().run(frames)
        counts = [tr.active_count(k) for k in range(tr.n_frames)]
        self.assertGreater(min(counts), 0)
        self.assertGreater(len(tr.tracks), max(counts))  # respawns happened

    def test_lost_points_not_in_records(self):
        frames, _ = small_frames(16, n_occlusions=2)
        tr = FeatureTracker().run(frames)
        for k in range(1, tr.n_frames):
            src, dst, ids = tr.matches(k)
            if len(ids):
                d = np.hypot(*(dst - src).T)
                self.assertLess(np.median(d), 40.0)  # no stale zero-jumps


class TestMotion(unittest.TestCase):
    def test_affine_recovers_transform_with_outliers(self):
        rng = np.random.default_rng(0)
        src = rng.uniform(0, 200, (60, 2))
        m_true = np.array([[0.999, -0.02, -8.0], [0.02, 0.999, 3.0]])
        dst = src @ m_true[:, :2].T + m_true[:, 2]
        dst[:10] += rng.uniform(-40, 40, (10, 2))  # outliers
        m, inl, _, rms = robust_affine(src, dst)
        self.assertGreater(inl.sum(), 40)
        self.assertLess(np.abs(m - m_true).max(), 0.05)

    def test_euclidean_translation_accuracy(self):
        rng = np.random.default_rng(1)
        src = rng.uniform(0, 200, (50, 2))
        dst = src + np.array([7.5, -3.0])
        m, _, _, _ = robust_euclidean(src, dst)
        self.assertLess(abs(m[0, 2] - 7.5), 0.2)
        self.assertLess(abs(m[1, 2] + 3.0), 0.2)

    def test_insufficient_points_raises(self):
        with self.assertRaises(InsufficientTracksError):
            robust_affine(np.zeros((2, 2)), np.zeros((2, 2)))

    def test_sanitize_flags_and_repairs(self):
        good = np.array([[1.0, 0.0, 5.0], [0.0, 1.0, 2.0]])
        bad = np.array([[0.7, 0.3, 50.0], [-0.3, 0.7, -40.0]])
        transforms = [good.copy() for _ in range(6)]
        transforms[3] = bad
        rms = [0.5, 0.5, 0.5, 20.0, 0.5, 0.5]
        clean, failures = sanitize_transforms(transforms, rms)
        self.assertEqual(len(failures), 1)
        self.assertLess(abs(clean[3][0, 2] - 5.0), 1.0)


class TestRollingShutter(unittest.TestCase):
    def test_correct_frame_undoes_row_shift(self):
        # smooth image: bilinear interpolation is near-lossless there
        yy, xx = np.mgrid[0:40, 0:60].astype(float)
        img = 128 + 60 * np.sin(xx / 6.0) * np.cos(yy / 5.0)
        ro = np.zeros((40, 2))
        ro[:, 1] = row_frac(40) * 6  # y shift linear in row
        shifted = frameio.bilinear_sample(img, yy - ro[:, 1:2], xx)
        back = correct_frame(shifted, ro)
        err_before = np.abs(shifted[4:-4] - img[4:-4]).mean()
        err_after = np.abs(back[4:-4] - img[4:-4]).mean()
        # correction must clearly reduce the row-shift distortion
        # (double bilinear interpolation leaves some blur behind)
        self.assertLess(err_after, err_before * 0.6)

    def test_estimate_row_delay_and_direction(self):
        rng = np.random.default_rng(2)
        # velocities as a jittery sequence; slopes follow rs * du
        vels = [rng.uniform(-20, 20, 2)]
        for _ in range(29):
            vels.append(vels[-1] + rng.uniform(-15, 15, 2))
        slopes = []
        for k in range(len(vels) - 1):
            du = vels[k + 1] - vels[k]
            slopes.append(0.4 * du + rng.normal(0, 0.3, 2))
        est = estimate_parameters(slopes, vels, 120)
        self.assertAlmostEqual(est["row_delay"], 0.4, delta=0.1)
        self.assertEqual(est["direction"], 1)
        self.assertTrue(est["significant"])


class TestSmoother(unittest.TestCase):
    def test_crop_constraint_holds(self):
        rng = np.random.default_rng(3)
        t = np.arange(60, dtype=float)
        raw = np.stack([20 * t + rng.normal(0, 12, 60),
                        rng.normal(0, 12, 60)], axis=1)
        from stabilizer.motion import params_pose
        poses = [params_pose(raw[i, 0], raw[i, 1], 0.0) for i in range(60)]
        smoothed, info = smooth_path(poses, (120, 160), max_crop=0.15)
        self.assertLessEqual(info["crop_ratio"], 0.15 + 1e-9)
        self.assertEqual(len(smoothed), 60)

    def test_detrend_handles_ramp_at_boundaries(self):
        t = np.arange(50, dtype=float)
        trend, resid = detrend(3.0 * t)
        self.assertLess(np.abs(resid).max(), 1.0)

    def test_jitter_reduced(self):
        rng = np.random.default_rng(4)
        t = np.arange(80, dtype=float)
        from stabilizer.motion import params_pose
        jitter = rng.normal(0, 10, (80, 2))
        raw = np.stack([5 * t, 0 * t], axis=1) + jitter
        poses = [params_pose(raw[i, 0], raw[i, 1], 0.0) for i in range(80)]
        smoothed, _ = smooth_path(poses, (120, 160))
        before = metrics_mod.jitter_px(poses)
        after = metrics_mod.jitter_px(smoothed)
        self.assertLess(after, before * 0.4)


class TestFrameIO(unittest.TestCase):
    def test_roundtrip_and_validation(self):
        with tempfile.TemporaryDirectory() as td:
            frames = np.zeros((3, 10, 12), dtype=np.uint8)
            frameio.save_frames(frames, td)
            loaded = frameio.load_frames(td)
            self.assertEqual(loaded.shape, (3, 10, 12))
            np.save(Path(td) / "frame_0003.npy", np.zeros((9, 12), np.uint8))
            with self.assertRaises(InputError):
                frameio.load_frames(td)

    def test_nan_frame_rejected(self):
        with tempfile.TemporaryDirectory() as td:
            f = np.zeros((10, 10))
            f[3, 3] = np.nan
            np.save(Path(td) / "frame_00000.npy", f)
            with self.assertRaises(InputError):
                frameio.load_frames(td)


class TestPipeline(unittest.TestCase):
    def test_demo_end_to_end(self):
        with tempfile.TemporaryDirectory() as td:
            rc = cli.main(["demo", "--frames", "40", "--width", "160",
                           "--height", "120", "--seed", "17", "--pan", "6",
                           "--rolling-shutter", "0.35", "--output", td])
            self.assertEqual(rc, 0)
            m = json.loads((Path(td) / "metrics.json").read_text())
            self.assertEqual(m["frames"], 40)
            self.assertGreater(m["tracked_points"], 20)
            self.assertLess(m["track_residual_px"], 2.5)
            self.assertLess(m["jitter_after_px"],
                            m["jitter_before_px"] * 0.5)
            self.assertLessEqual(m["crop_ratio"], 0.18)
            self.assertTrue(m["rolling_shutter_corrected"])
            self.assertTrue((Path(td) / "trajectories.json").exists())
            self.assertTrue((Path(td) / "stabilized").is_dir())

    def test_track_then_stabilize_reuses_trajectory(self):
        with tempfile.TemporaryDirectory() as td:
            frames, _ = small_frames(20, rolling_shutter=0.3)
            fdir = Path(td) / "frames"
            frameio.save_frames(frames, fdir)
            traj = Path(td) / "traj.json"
            self.assertEqual(cli.main(["track", "--frames-dir", str(fdir),
                                       "--output", str(traj)]), 0)
            out = Path(td) / "out"
            self.assertEqual(cli.main(["stabilize", "--frames-dir",
                                       str(fdir), "--trajectory", str(traj),
                                       "--output", str(out)]), 0)
            self.assertTrue((out / "stabilized").is_dir())

    def test_exit_code_on_nan_input(self):
        with tempfile.TemporaryDirectory() as td:
            f = np.zeros((2, 30, 30))
            f[0, 5, 5] = np.nan
            np.save(Path(td) / "frame_00000.npy", f[0])
            np.save(Path(td) / "frame_00001.npy", f[1])
            np.save(Path(td) / "frame_00002.npy", f[1])
            rc = cli.main(["track", "--frames-dir", td,
                           "--output", str(Path(td) / "t.json")])
            self.assertEqual(rc, 2)

    def test_exit_code_insufficient_tracks(self):
        with tempfile.TemporaryDirectory() as td:
            for i in range(4):
                np.save(Path(td) / f"frame_{i:05d}.npy",
                        np.full((60, 80), 128, np.uint8))
            rc = cli.main(["track", "--frames-dir", td,
                           "--output", str(Path(td) / "t.json")])
            self.assertEqual(rc, 3)

    def test_exit_code_missing_input(self):
        rc = cli.main(["track", "--frames-dir", "/nonexistent-dir",
                       "--output", "/tmp/never.json"])
        self.assertEqual(rc, 2)


if __name__ == "__main__":
    unittest.main()
