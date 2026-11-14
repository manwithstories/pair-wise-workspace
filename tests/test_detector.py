import numpy as np

from audiocut.detector import DetectorParams, detect

SR = 48000
PARAMS = DetectorParams(sample_rate=SR, frame=960, hop=480)


def make_env(spec, n, floor=-60.0):
    env = np.full(n, floor)
    for s, e, level in spec:
        env[s:e] = level
    return env, np.full(n, floor)


def test_hysteresis_basic():
    env, floor = make_env([(100, 140, -45.0), (300, 360, -45.0)], 500)
    segs, sils = detect(env, floor, PARAMS, total_samples=500 * 480)
    assert len(segs) == 2
    assert segs[0].start_sample == 100 * 480
    assert segs[0].end_sample == 140 * 480 + 960
    covered = sorted([(s.start_sample, s.end_sample) for s in segs + sils])
    assert covered[0][0] == 0 and covered[-1][1] == 500 * 480
    for (a_s, a_e), (b_s, _b_e) in zip(covered, covered[1:]):
        assert a_e == b_s


def test_min_silence_merge():
    env, floor = make_env([(100, 120, -45.0), (130, 150, -45.0)], 400)  # 100ms gap
    segs, _ = detect(env, floor, PARAMS, total_samples=400 * 480)
    assert len(segs) == 1
    env, floor = make_env([(100, 120, -45.0), (160, 180, -45.0)], 400)  # 400ms gap
    segs, _ = detect(env, floor, PARAMS, total_samples=400 * 480)
    assert len(segs) == 2


def test_fade_rollback_and_limit():
    n = 400
    env = np.full(n, -60.0)
    env[90:100] = np.linspace(-59, -50, 10)   # gradual fade-in above floor+6
    env[100:140] = -45.0
    floor = np.full(n, -60.0)
    segs, _ = detect(env, floor, PARAMS, total_samples=n * 480)
    assert segs[0].start_sample == 95 * 480  # rolled back to floor+6dB crossing
    env2 = np.full(n, -60.0)
    env2[70:100] = np.linspace(-53, -50, 30)  # 300ms fade above floor+6dB: capped
    env2[100:140] = -45.0
    segs2, _ = detect(env2, floor, PARAMS, total_samples=n * 480)
    assert segs2[0].start_sample == (90 - 15) * 480  # hysteresis at 90, cap 15 frames


def test_short_syllable_not_eaten():
    env, floor = make_env([(100, 108, -40.0)], 300)  # 80ms but strong peak
    segs, _ = detect(env, floor, PARAMS, total_samples=300 * 480)
    assert len(segs) == 1
    env, floor = make_env([(100, 108, -52.0)], 300)  # 80ms, weak blip
    segs, _ = detect(env, floor, PARAMS, total_samples=300 * 480)
    assert len(segs) == 0


def test_low_snr_segments_not_merged():
    spec = [(50 + i * 60, 80 + i * 60, -48.0) for i in range(6)]  # speech -48, floor -60
    env, floor = make_env(spec, 500)
    segs, _ = detect(env, floor, PARAMS, total_samples=500 * 480)
    assert len(segs) == 6


def test_all_silence_and_all_speech():
    env = np.full(200, -120.0)
    segs, sils = detect(env, np.full(200, -120.0), PARAMS, total_samples=200 * 480)
    assert segs == [] and len(sils) == 1
    env = np.full(200, -30.0)
    segs, _ = detect(env, np.full(200, -45.0), PARAMS, total_samples=200 * 480)
    assert len(segs) == 1 and segs[0].start_sample == 0
