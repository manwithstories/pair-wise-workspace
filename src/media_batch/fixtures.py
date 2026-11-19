"""Deterministic sample-data generator (fixed seed, no external files).

Produces, under <workdir>/input/:
  program.wav        48 kHz mono s16 program with tones, silence gaps, duplicates
  program.srt        120 valid cues + invalid-time, truncated and overlapping records
  program.vtt        small WebVTT with one illegal timestamp
  timecodes.txt      29.97 drop-frame timecodes crossing midnight, with a duplicate
  timecodes_25.txt   25 fps timecodes that land inside the program (cut points)
  program.mp4        full-length VFR H.264/AAC container for probing
  video_only.mp4     video stream without audio (missing-audio-track error)
  truncated.mp4      byte-truncated program.mp4 (bad-packet error path)
  drift.mka          audio whose container clock runs 180 ms/min fast
  broken.mp4         undecodable bytes (bad-container error path)
"""

from __future__ import annotations

from fractions import Fraction
from pathlib import Path

import numpy as np
import soundfile as sf

SEED = 12785
RATE = 48000
N_CUES = 120
CUE_MS = 1500
CUE_START_PAD_MS = 100
CUE_BODY_MS = 1200
VIDEO_FPS = 24
VIDEO_TICKS = 120           # time base 1/120: alternating 4/6 tick spacing -> VFR
DRIFT_MS_PER_MINUTE = 180  # > 100 ms/min threshold -> untrusted

DUP_A = (30, 80)    # identical content
DUP_B = (40, 95)    # same content, lower gain + leading silence
LONG_SILENCE_CUES = (10, 50, 90)   # 500 ms leading silence (reported)
SHORT_SILENCE_CUE = 70             # 150 ms leading silence (not reported)


def _tone(freq: float, ms: int, gain: float = 0.3) -> np.ndarray:
    t = np.arange(int(RATE * ms / 1000)) / RATE
    return (np.sin(2 * np.pi * freq * t) * gain * 32767).astype(np.int16)


def _silence(ms: int) -> np.ndarray:
    return np.zeros(int(RATE * ms / 1000), dtype=np.int16)


# Each cue is a single carrier 60 Hz above the previous one (200 Hz to
# ~7.3 kHz, inside the fingerprint's 8 kHz analysis band), so every
# non-duplicate cue is spectrally distinct. Two deliberate duplicates reuse
# another cue's carrier with different gain and edge silence.
_CARRIERS = tuple(200 + 60 * k for k in range(N_CUES))


def _carrier(i: int, ms: int, gain: float = 0.3) -> np.ndarray:
    return _tone(_CARRIERS[i], ms, gain)


def _cue_body(i: int) -> np.ndarray:
    if i == DUP_A[0] or i == DUP_A[1]:
        return _carrier(DUP_A[0], CUE_BODY_MS)
    if i == DUP_B[0]:
        return np.concatenate([_carrier(DUP_B[0], 1000), _silence(200)])
    if i == DUP_B[1]:
        return np.concatenate([_silence(200), _carrier(DUP_B[0], 1000, gain=0.15)])
    return _carrier(i, CUE_BODY_MS)


def _lead_ms(i: int) -> int:
    if i in LONG_SILENCE_CUES:
        return 500
    if i == SHORT_SILENCE_CUE:
        return 150
    return 0


def build_audio() -> np.ndarray:
    parts: list[np.ndarray] = []
    for i in range(N_CUES):
        lead = _lead_ms(i)
        body = _cue_body(i)[: int(RATE * (CUE_BODY_MS - lead) / 1000)] \
            if lead else _cue_body(i)
        parts.append(_silence(lead))
        parts.append(body)
        parts.append(_silence(CUE_MS - lead - len(body) * 1000 // RATE))
    # One extra cue's worth of tail so the program always outlasts the video
    # (VFR video may end slightly early); the tail is silence.
    parts.append(_silence(CUE_MS))
    return np.concatenate(parts)


def _fmt_srt_ms(ms: int) -> str:
    h, rem = divmod(ms, 3_600_000)
    m, rem = divmod(rem, 60_000)
    s, milli = divmod(rem, 1000)
    return f"{h:02d}:{m:02d}:{s:02d},{milli:03d}"


def cue_window(i: int) -> tuple[int, int]:
    lead = _lead_ms(i)
    start = i * CUE_MS + lead
    return start, start + CUE_BODY_MS - lead


def build_srt() -> str:
    blocks: list[str] = []
    n = 1
    for i in range(N_CUES):
        s, e = cue_window(i)
        blocks.append(f"{n}\n{_fmt_srt_ms(s)} --> {_fmt_srt_ms(e)}\ncue {i} text\n")
        n += 1
        if i == 5:  # overlapping record (valid timing, overlaps cue 5)
            s5, e5 = cue_window(5)
            blocks.append(f"{n}\n{_fmt_srt_ms(e5 - 500)} --> {_fmt_srt_ms(e5 + 400)}\n"
                          f"overlapping pickup line\n")
            n += 1
        if i == 20:  # reversed/illegal timing
            blocks.append(f"{n}\n00:02:00,000 --> 00:01:59,000\nbad timing line\n")
            n += 1
        if i == 60:  # truncated record: index only, no timing line
            blocks.append(f"{n}\n")
            n += 1
    # final record cut off mid timing line (no terminating newline)
    blocks.append(f"{n}\n00:05:00,000 -->\ncut off pickup")
    return "\n".join(blocks)


def build_vtt() -> str:
    return ("WEBVTT\n\n"
            "1\n00:00.000 --> 00:01.200\nvtt cue one\n\n"
            "2\n00:9x.000 --> 00:02.000\nvtt broken timing\n\n"
            "3\n00:02.500 --> 00:03.000\nvtt cue three\n")


def build_timecodes() -> str:
    codes = ["23:59:58;00", "23:59:59;00", "23:59:59;29",
             "00:00:00;00", "00:00:01;00", "00:00:01;00", "00:00:02;00"]
    return "# fps=29.97\n" + "\n".join(codes) + "\n"


def build_cut_timecodes() -> str:
    """25 fps cut points inside non-duplicate cues 2, 60 and 100.

    Cues are 1.5 s long, so a cut at +760 ms falls inside the cue body and must
    split it into two pieces without losing or repeating audio.
    """
    frames = [3 * 25 + 19, 60 * 25 + 19, 100 * 25 + 19]  # 3760, 60760, 100760 ms
    codes = [f"{f // (25 * 3600):02d}:{f // (25 * 60) % 60:02d}:"
             f"{f // 25 % 60:02d}:{f % 25:02d}" for f in frames]
    return "# fps=25\n" + "\n".join(codes) + "\n"


def build_mp4(path: Path, seconds: float, with_audio: bool = True,
              faststart: bool = False) -> None:
    """Variable-frame-rate video with alternating 4/6-tick spacing (24 fps peak).

    `faststart` puts the moov atom first so the file can be byte-truncated and
    still be demuxed up to the cut (a bad-packet path, not a bad container).
    """
    import av

    n_frames = int(seconds * VIDEO_FPS)
    options = {"movflags": "faststart"} if faststart else {}
    container = av.open(str(path), "w", options=options)
    vstream = container.add_stream("mpeg4", rate=VIDEO_TICKS)
    vstream.width, vstream.height, vstream.pix_fmt = 64, 64, "yuv420p"
    astream = None
    if with_audio:
        astream = container.add_stream("aac", rate=RATE)
        astream.layout = "mono"
    pts = 0
    for i in range(n_frames):
        img = np.full((64, 64, 3), (i * 3) % 255, dtype=np.uint8)
        frame = av.VideoFrame.from_ndarray(img, format="rgb24")
        frame.pts = pts
        pts += 4 if i % 2 == 0 else 6  # mean 5 ticks = 24 fps, alternating -> VFR
        for pkt in vstream.encode(frame):
            container.mux(pkt)
    for pkt in vstream.encode():
        container.mux(pkt)
    if astream is not None:
        t = np.arange(int(RATE * seconds)) / RATE
        audio = (np.sin(2 * np.pi * 330 * t) * 12000).astype(np.int16)
        for off in range(0, len(audio), 1024):
            aframe = av.AudioFrame.from_ndarray(
                audio[off:off + 1024].reshape(1, -1), format="s16", layout="mono")
            aframe.sample_rate = RATE
            for pkt in astream.encode(aframe):
                container.mux(pkt)
        for pkt in astream.encode():
            container.mux(pkt)
    container.close()


def build_drift_mka(path: Path, seconds: float = 20.0) -> None:
    """PCM audio in Matroska whose container pts runs fast by a fixed rate."""
    import av

    t = np.arange(int(RATE * seconds)) / RATE
    sig = (np.sin(2 * np.pi * 330 * t) * 12000).astype(np.int16)
    container = av.open(str(path), "w")
    stream = container.add_stream("pcm_s16le", rate=RATE)
    stream.layout = "mono"
    for start in range(0, len(sig), 1024):
        frame = av.AudioFrame.from_ndarray(
            sig[start:start + 1024].reshape(1, -1), format="s16", layout="mono")
        frame.sample_rate = RATE
        frame.pts = start + (start * DRIFT_MS_PER_MINUTE) // 60_000
        for pkt in stream.encode(frame):
            if pkt.pts is None:
                pkt.pts = frame.pts
            if pkt.dts is None:
                pkt.dts = pkt.pts
            container.mux(pkt)
    for pkt in stream.encode():
        container.mux(pkt)
    container.close()


def generate(workdir: str | Path) -> dict:
    workdir = Path(workdir)
    inp = workdir / "input"
    inp.mkdir(parents=True, exist_ok=True)
    rng = np.random.default_rng(SEED)
    audio = build_audio()
    seconds = len(audio) / RATE
    sf.write(str(inp / "program.wav"), audio, RATE, subtype="PCM_16")
    (inp / "program.srt").write_text(build_srt(), encoding="utf-8")
    (inp / "program.vtt").write_text(build_vtt(), encoding="utf-8")
    (inp / "timecodes.txt").write_text(build_timecodes(), encoding="utf-8")
    (inp / "timecodes_25.txt").write_text(build_cut_timecodes(), encoding="utf-8")
    build_mp4(inp / "program.mp4", seconds)
    build_mp4(inp / "video_only.mp4", min(seconds, 10.0), with_audio=False)
    cut = inp / "truncated.mp4"
    build_mp4(cut, min(seconds, 10.0), faststart=True)
    raw = cut.read_bytes()
    cut.write_bytes(raw[: len(raw) * 4 // 5])
    build_drift_mka(inp / "drift.mka", min(seconds, 20.0))
    (inp / "broken.mp4").write_bytes(rng.bytes(2048))
    return {"workdir": str(workdir), "cues": N_CUES, "seed": SEED,
            "wav_seconds": round(seconds, 3),
            "video_seconds": round(seconds, 3),
            "drift_ms_per_minute": DRIFT_MS_PER_MINUTE}
