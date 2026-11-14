"""Audio track synthesis, per-frame RMS windows, and A/V alignment error."""

import math
import wave

import numpy as np

from .errors import AssetError

SAMPLE_RATE = 44100


def synth_wav(path, kind, duration, seed=0, sample_rate=SAMPLE_RATE):
    """Write a deterministic 16-bit mono PCM WAV ("voice" or "music")."""
    rng = np.random.default_rng(seed)
    n = int(round(duration * sample_rate))
    t = np.arange(n, dtype=np.float64) / sample_rate
    if kind == "voice":
        # syllable-like AM bursts over a wandering fundamental
        f0 = 110.0 + 40.0 * np.sin(2.0 * np.pi * 0.35 * t)
        carrier = np.sin(2.0 * np.pi * np.cumsum(f0) / sample_rate)
        gate = (np.sin(2.0 * np.pi * 2.2 * t + rng.uniform(0, 0.01)) > -0.2)
        env = gate * (0.5 + 0.5 * np.sin(2.0 * np.pi * 5.0 * t))
        sig = 0.6 * env * carrier
    elif kind == "music":
        sig = np.zeros(n)
        for freq, amp in ((220.0, 0.30), (277.18, 0.22), (329.63, 0.18)):
            sig += amp * np.sin(2.0 * np.pi * freq * t)
        sig *= 0.7 + 0.3 * np.sin(2.0 * np.pi * 0.5 * t)
    else:
        raise AssetError("unknown synth kind %r (expected voice|music)" % kind)
    sig += rng.uniform(-0.002, 0.002, n)  # deterministic dither floor
    pcm = np.clip(sig, -1.0, 1.0)
    pcm = np.rint(pcm * 32767.0).astype("<i2")
    with wave.open(path, "wb") as fh:
        fh.setnchannels(1)
        fh.setsampwidth(2)
        fh.setframerate(sample_rate)
        fh.writeframes(pcm.tobytes())
    return path


def read_wav(path):
    """Read a mono/stereo 16-bit PCM WAV into (float32 samples, rate)."""
    try:
        with wave.open(path, "rb") as fh:
            channels = fh.getnchannels()
            width = fh.getsampwidth()
            rate = fh.getframerate()
            raw = fh.readframes(fh.getnframes())
    except FileNotFoundError:
        raise AssetError("audio asset not found: %s" % path)
    except (wave.Error, EOFError, OSError) as exc:
        raise AssetError("audio asset is corrupt: %s (%s)" % (path, exc))
    if width != 2:
        raise AssetError("unsupported sample width %d in %s" % (width, path))
    data = np.frombuffer(raw, dtype="<i2").astype(np.float32) / 32768.0
    if channels > 1:
        data = data.reshape(-1, channels).mean(axis=1)
    return data, rate


def mix_tracks(clips, assets_dir, duration, sample_rate=SAMPLE_RATE):
    """Mix timeline audio clips into a single float32 mono buffer."""
    import os

    n = int(round(duration * sample_rate))
    mix = np.zeros(n, dtype=np.float32)
    for clip in clips:
        data, rate = read_wav(os.path.join(assets_dir, clip.asset))
        if rate != sample_rate:
            # deterministic nearest-neighbour resample to the mix rate
            idx = np.minimum(
                np.arange(int(round(len(data) * sample_rate / rate)))
                * rate // sample_rate, len(data) - 1)
            data = data[idx]
        offset = int(round(clip.start * sample_rate))
        end = min(offset + len(data), n)
        if end > offset:
            mix[offset:end] += (data[:end - offset] * clip.gain).astype(np.float32)
    return mix


def frame_rms(mix, fps, n_frames, sample_rate=SAMPLE_RATE):
    """RMS of each frame's sample window (window = 1/fps seconds)."""
    out = []
    for i in range(n_frames):
        a = int(round(i * sample_rate / fps))
        b = int(round((i + 1) * sample_rate / fps))
        window = mix[a:min(b, len(mix))]
        if window.size == 0:
            out.append(0.0)
        else:
            out.append(float(np.sqrt(np.mean(window.astype(np.float64) ** 2))))
    return out


def alignment_errors(clips, fps):
    """Per-clip offset between scheduled start and the nearest frame edge.

    Returns (max_ms, mean_ms); both are 0.0 when every clip starts exactly
    on a frame boundary.
    """
    if not clips:
        return 0.0, 0.0
    frame = 1.0 / fps
    errs = []
    for clip in clips:
        nearest = round(clip.start / frame) * frame
        errs.append(abs(clip.start - nearest) * 1000.0)
    return max(errs), sum(errs) / len(errs)
