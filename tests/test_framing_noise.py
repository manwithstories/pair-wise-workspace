import numpy as np

from audiocut import framing, noise


def test_frame_params_follow_sample_rate():
    assert framing.frame_params(48000, 20, 10) == (960, 480)
    assert framing.frame_params(44100, 20, 10) == (882, 441)
    assert framing.frame_params(8000, 20, 10) == (160, 80)


def test_envelope_known_level():
    sr = 48000
    frame, hop = framing.frame_params(sr)
    amp = 10 ** (-20.0 / 20.0)  # -20 dBFS RMS sine
    sig = amp * np.sqrt(2) * np.sin(2 * np.pi * 440 * np.arange(sr) / sr)
    res = framing.envelope_dbfs([sig], frame, hop)
    assert np.all(np.abs(res.env_db - (-20.0)) < 0.1)
    assert res.total_samples == sr


def test_envelope_all_zero_and_tail():
    frame, hop = 960, 480
    sig = np.zeros(frame + hop + 100)  # 2 full frames + 100-sample tail
    res = framing.envelope_dbfs([sig[:1000], sig[1000:]], frame, hop)
    assert res.env_db.size == 2
    assert np.all(res.env_db == framing.FLOOR_DB)
    assert res.tail_samples == 100
    assert res.tail_db == framing.FLOOR_DB


def test_envelope_streaming_matches_single_block():
    rng = np.random.default_rng(3)
    sig = rng.normal(0, 0.1, 50000)
    frame, hop = 960, 480
    whole = framing.envelope_dbfs([sig], frame, hop)
    chunked = framing.envelope_dbfs(
        [sig[i:i + 1234] for i in range(0, sig.size, 1234)], frame, hop)
    assert np.array_equal(whole.env_db, chunked.env_db)


def test_noise_floor_tracks_drift():
    rng = np.random.default_rng(4)
    n = 2000
    drift = np.linspace(-72, -54, n)
    env = drift + rng.normal(0, 0.5, n)
    speech = rng.choice(n, 300, replace=False)
    env[speech] += 25.0
    floor = noise.estimate_noise_floor(env, chunk_frames=100)
    assert floor.shape == (n,)
    err = np.abs(np.delete(floor, speech) - np.delete(drift, speech))
    assert np.percentile(err, 90) < 2.0


def test_noise_floor_degenerate_inputs():
    silent = noise.estimate_noise_floor(np.full(50, framing.FLOOR_DB))
    assert np.all(silent == framing.FLOOR_DB)
    speech = noise.estimate_noise_floor(np.full(50, -30.0))
    assert np.all(speech < -30.0)  # floor sits below continuous speech
    assert noise.estimate_noise_floor(np.empty(0)).size == 0
