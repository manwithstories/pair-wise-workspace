"""Deterministic sample-data generator (fixed seed, no external files).

Produces, under <workdir>/input/:
  program.wav    48 kHz mono s16 program with tones, silence gaps, duplicates
  program.srt    120 valid cues + invalid-time, truncated and overlapping records
  program.vtt    small WebVTT with one illegal timestamp
  timecodes.txt  29.97 drop-frame timecodes crossing midnight, with a duplicate
  program.mp4    small VFR H.264/AAC container for probing
  broken.mp4     undecodable bytes for the bad-container error path
"""

from __future__ import annotations

from pathlib import Path

import numpy as np
import soundfile as sf

SEED = 12785
RATE = 48000
N_CUES = 120
CUE_MS = 1500
CUE_START_PAD_MS = 100
CUE_BODY_MS = 1200

DUP_A = (30, 80)    # identical content
DUP_B = (40, 95)    # same content, lower gain + leading silence
LONG_SILENCE_CUES = (10, 50, 90)   # 500 ms leading silence (reported)
SHORT_SILENCE_CUE = 70             # 150 ms leading silence (not reported)


def _tone(freq: float, ms: int, gain: float = 0.3) -> np.ndarray:
    t = np.arange(int(RATE * ms / 1000)) / RATE
    return (np.sin(2 * np.pi * freq * t) * gain * 32767).astype(np.int16)


def _silence(ms: int) -> np.ndarray:
    return np.zeros(int(RATE * ms / 1000), dtype=np.int16)


def _cue_body(i: int) -> np.ndarray:
    if i == DUP_A[0] or i == DUP_A[1]:
        return _tone(440.0, CUE_BODY_MS)
    if i == DUP_B[0]:
        return _tone(550.0, CUE_BODY_MS)
    if i == DUP_B[1]:
        return np.concatenate([_silence(200), _tone(550.0, CUE_BODY_MS - 200, gain=0.15)])
    freq = 220.0 + (i * 37) % 660
    return _tone(freq, CUE_BODY_MS)


def build_audio() -> np.ndarray:
    parts: list[np.ndarray] = []
    for i in range(N_CUES):
        if i in LONG_SILENCE_CUES:
            parts.append(_silence(500))
        elif i == SHORT_SILENCE_CUE:
            parts.append(_silence(150))
        parts.append(_cue_body(i))
        parts.append(_silence(CUE_MS - CUE_BODY_MS - 500
                              if i in LONG_SILENCE_CUES else
                              CUE_MS - CUE_BODY_MS - 150
                              if i == SHORT_SILENCE_CUE else
                              CUE_MS - CUE_BODY_MS))
    return np.concatenate(parts)


def _fmt_srt_ms(ms: int) -> str:
    h, rem = divmod(ms, 3_600_000)
    m, rem = divmod(rem, 60_000)
    s, milli = divmod(rem, 1000)
    return f"{h:02d}:{m:02d}:{s:02d},{milli:03d}"


def cue_window(i: int) -> tuple[int, int]:
    start = i * CUE_MS + (500 if i in LONG_SILENCE_CUES else
                          150 if i == SHORT_SILENCE_CUE else 0)
    return start, start + CUE_BODY_MS


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
    return "\n".join(blocks)


def build_vtt() -> str:
    return ("WEBVTT\n\n"
            "1\n00:00.000 --> 00:01.200\nvtt cue one\n\n"
            "2\n00:9x.000 --> 00:02.000\nvtt broken timing\n\n"
            "3\n00:02.500 --> 00:03.000\nvtt cue three\n")


def build_timecodes() -> str:
    from .timestamps import timecode_to_frames  # noqa: F401  (consistency check)
    codes = ["23:59:58;00", "23:59:59;00", "23:59:59;29",
             "00:00:00;00", "00:00:01;00", "00:00:01;00", "00:00:02;00"]
    return "\n".join(codes) + "\n"


def build_mp4(path: Path) -> None:
    import av
    container = av.open(str(path), "w")
    vstream = container.add_stream("mpeg4", rate=24)
    vstream.width, vstream.height, vstream.pix_fmt = 64, 64, "yuv420p"
    astream = container.add_stream("aac", rate=RATE)
    astream.layout = "mono"
    pts = 0
    for i in range(96):  # 4 s of video, irregular pts -> VFR
        img = np.full((64, 64, 3), (i * 3) % 255, dtype=np.uint8)
        frame = av.VideoFrame.from_ndarray(img, format="rgb24")
        frame.pts = pts
        pts += 1 + (1 if i % 9 == 0 else 0)
        for pkt in vstream.encode(frame):
            container.mux(pkt)
    for pkt in vstream.encode():
        container.mux(pkt)
    t = np.arange(RATE * 4) / RATE
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


def generate(workdir: str | Path) -> dict:
    workdir = Path(workdir)
    inp = workdir / "input"
    inp.mkdir(parents=True, exist_ok=True)
    rng = np.random.default_rng(SEED)
    audio = build_audio()
    sf.write(str(inp / "program.wav"), audio, RATE, subtype="PCM_16")
    (inp / "program.srt").write_text(build_srt(), encoding="utf-8")
    (inp / "program.vtt").write_text(build_vtt(), encoding="utf-8")
    (inp / "timecodes.txt").write_text(build_timecodes(), encoding="utf-8")
    build_mp4(inp / "program.mp4")
    (inp / "broken.mp4").write_bytes(rng.bytes(2048))
    return {"workdir": str(workdir), "cues": N_CUES, "seed": SEED,
            "wav_seconds": round(len(audio) / RATE, 3)}
