#!/usr/bin/env python3
"""Generate all sample media fixtures deterministically (no external files)."""

import argparse
import os
import random
import struct
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from mediaqa.packager import write_mp4, write_wav  # noqa: E402

ROT90 = [0, 0x10000, 0, -0x10000, 0, 0, 0, 0, 0x40000000]


def video_track(rnd, track_id, frames, language="eng", matrix=None,
                elst=None):
    sizes = [rnd.randint(180, 480) for _ in range(frames)]
    return {"track_id": track_id, "kind": "video", "codec": "avc1",
            "timescale": 30000, "ticks": frames * 1000, "sample_count": frames,
            "sizes": sizes, "data": rnd.randbytes(sum(sizes)),
            "language": language, "width": 1920, "height": 1080,
            "matrix": matrix, "elst": elst}


def audio_track(rnd, track_id, seconds, language="zho"):
    samples = 4800 * seconds
    return {"track_id": track_id, "kind": "audio", "codec": "mp4a",
            "timescale": 48000, "ticks": samples * 10, "sample_count": samples,
            "sizes": [24] * samples, "data": rnd.randbytes(samples * 24),
            "language": language, "sample_rate": 48000, "channels": 2,
            "bits": 16}


def subtitle_track(rnd, track_id, seconds, language="und"):
    return {"track_id": track_id, "kind": "subtitle", "codec": "tx3g",
            "timescale": 1000, "ticks": seconds * 1000, "sample_count": seconds,
            "sizes": [8] * seconds, "data": rnd.randbytes(8 * seconds),
            "language": language}


# ----------------------------------------------------------------- WebM raw

def _eid(eid):
    return eid.to_bytes((eid.bit_length() + 7) // 8, "big")


def _esize(n):
    for length in range(1, 9):
        if n < (1 << (7 * length)) - 1:
            return ((1 << (7 * length)) | n).to_bytes(length, "big")
    raise ValueError(n)


def _el(eid, payload):
    return _eid(eid) + _esize(len(payload)) + payload


def _uint(eid, value):
    return _el(eid, value.to_bytes(max(1, (value.bit_length() + 7) // 8), "big"))


def _text(eid, text):
    return _el(eid, text.encode())


def _track_entry(number, kind, codec, extra=b"", language="eng"):
    body = _uint(0xD7, number) + _uint(0x83, kind) + _text(0x86, codec)
    return _el(0xAE, body + _text(0x22B59C, language) + extra)


def webm_bytes(with_duration, unknown_segment, tracks_payload, last_timecode):
    header = _el(0x1A45DFA3, _uint(0x4286, 1) + _uint(0x42F7, 1)
                 + _uint(0x42F2, 4) + _uint(0x42F3, 8) + _text(0x4282, "webm")
                 + _uint(0x4287, 4) + _uint(0x4285, 2))
    info = _uint(0x2AD7B1, 1_000_000)
    if with_duration:
        info += _el(0x4489, struct.pack(">d", 5000.0))
    segment = _el(0x1549A966, info) + _el(0x1654AE6B, tracks_payload)
    segment += _el(0x1F43B675, _uint(0xE7, last_timecode))
    if unknown_segment:
        return header + _eid(0x18538067) + b"\xff" + segment
    return header + _el(0x18538067, segment)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", required=True)
    parser.add_argument("--seed", type=int, default=7)
    args = parser.parse_args()
    rnd = random.Random(args.seed)
    os.makedirs(args.out, exist_ok=True)
    made = []

    def wav(name, rate, channels, bits, seconds, **kw):
        frames = rate * seconds
        data = rnd.randbytes(frames * channels * bits // 8)
        path = os.path.join(args.out, name)
        write_wav(path, rate, channels, bits, data, **kw)
        made.append(name)

    def mp4(name, tracks, **kw):
        write_mp4(os.path.join(args.out, name), tracks, **kw)
        made.append(name)

    wav("wav_recpen_44k_mono24.wav", 44100, 1, 24, 4)
    wav("wav_guide_48k_stereo.wav", 48000, 2, 16, 5)
    wav("wav_extensible_fact.wav", 48000, 2, 16, 5, extensible=True,
        fact_frames=48000 * 5, extra_chunks=[(b"JUNK", b"abc")])

    mp4("mp4_standard.mp4",
        [video_track(rnd, 1, 150), audio_track(rnd, 2, 5)], moov_first=True)
    mp4("mp4_moov_last.mp4",
        [video_track(rnd, 1, 150), audio_track(rnd, 2, 5)])
    mp4("mp4_large_co64.mp4",
        [video_track(rnd, 1, 150), audio_track(rnd, 2, 5)],
        co64=True, mdat_largesize=True)
    mp4("mp4_unknown_brand.mp4",
        [video_track(rnd, 1, 150), audio_track(rnd, 2, 5)], brand="xyzz")
    mp4("mp4_ep01_video.mp4",
        [video_track(rnd, 1, 151, elst=[(151000, 3000)]),
         subtitle_track(rnd, 2, 5), audio_track(rnd, 3, 5)],
        brand="xyzz", mv_timescale=30000, moov_first=True)
    mp4("mov_rotation90.mov",
        [video_track(rnd, 1, 150, matrix=ROT90), audio_track(rnd, 2, 5)],
        brand="qt  ")
    mp4("mov_elst.mov",
        [video_track(rnd, 1, 150, elst=[(5000, 15000)]),
         audio_track(rnd, 2, 5)], brand="qt  ")

    v_extra = _uint(0x23E383, 33333333) + _el(0xE0, _uint(0xB0, 1920)
                                              + _uint(0xBA, 1080))
    a_extra = _el(0xE1, _el(0xB5, struct.pack(">d", 48000.0))
                  + _uint(0x9F, 2) + _uint(0x6264, 16))
    full = (_track_entry(1, 1, "V_VP9", v_extra)
            + _track_entry(2, 2, "A_OPUS", a_extra, "zho")
            + _track_entry(3, 8, "S_TEXT_UTF8", language="zho"))
    with open(os.path.join(args.out, "webm_full.webm"), "wb") as fh:
        fh.write(webm_bytes(True, False, full, 0))
    made.append("webm_full.webm")
    part = (_track_entry(1, 1, "V_VP8", v_extra)
            + _track_entry(2, 2, "A_OPUS", a_extra))
    with open(os.path.join(args.out, "webm_unknown.webm"), "wb") as fh:
        fh.write(webm_bytes(False, True, part, 4980))
    made.append("webm_unknown.webm")

    ftyp = struct.pack(">I4s", 16, b"ftyp") + b"isom" + b"\0" * 4
    with open(os.path.join(args.out, "truncated.mp4"), "wb") as fh:
        fh.write(ftyp + struct.pack(">I4s", 0x00010000, b"moov") + b"\0" * 16)
    made.append("truncated.mp4")
    with open(os.path.join(args.out, "fake.mp4"), "wb") as fh:
        fh.write(b"This is not a media file, just a renamed text file.\n")
    made.append("fake.mp4")

    print("generated %d fixtures in %s" % (len(made), args.out))
    for name in made:
        print("  %s" % name)


if __name__ == "__main__":
    main()
