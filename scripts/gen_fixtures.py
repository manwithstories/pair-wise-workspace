#!/usr/bin/env python3
"""Generate self-contained fMP4 fixtures for mp4pack tests."""

import argparse
import os
import struct

WRAP = 1 << 32
VIDEO_TIMESCALE = 90000
AUDIO_TIMESCALE = 48000
VIDEO_SEGMENT_DURATION = 6000
AUDIO_SAMPLE_DURATION = 1024
AUDIO_SAMPLES = 24
AUDIO_DTS_STEP = AUDIO_SAMPLES * AUDIO_SAMPLE_DURATION + 16
VIDEO_START = WRAP - 3 * VIDEO_SEGMENT_DURATION
AUDIO_START = WRAP - 3 * AUDIO_DTS_STEP


def u16(value):
    return struct.pack(">H", value)


def u24(value):
    return struct.pack(">I", value)[1:]


def u32(value):
    return struct.pack(">I", value & 0xFFFFFFFF)


def i32(value):
    return struct.pack(">i", value)


def u64(value):
    return struct.pack(">Q", value)


def box(box_type, payload):
    return u32(8 + len(payload)) + box_type.encode("ascii") + payload


def box64(box_type, payload):
    return u32(1) + box_type.encode("ascii") + u64(16 + len(payload)) + payload


def fullbox(box_type, version, flags, payload):
    return box(box_type, bytes([version]) + u24(flags) + payload)


def make_ftyp(major="isom", brands=("isom", "iso6", "dash")):
    return box("ftyp", major.encode("ascii") + u32(0x200) + b"".join(b.encode("ascii") for b in brands))


def make_mvhd(timescale=1000, duration=120000):
    matrix = (u32(0x00010000) + u32(0) + u32(0) + u32(0) + u32(0x00010000) +
              u32(0) + u32(0) + u32(0) + u32(0x40000000))
    payload = (u32(0) + u32(0) + u32(timescale) + u32(duration) + u32(0x00010000) +
               u16(0x0100) + u16(0) + b"\0" * 8 + matrix + b"\0" * 24 + u32(3))
    return fullbox("mvhd", 0, 0, payload)


def make_tkhd(track_id, duration=120000, width=0, height=0):
    matrix = (u32(0x00010000) + u32(0) + u32(0) + u32(0) + u32(0x00010000) +
              u32(0) + u32(0) + u32(0) + u32(0x40000000))
    payload = (u32(0) + u32(0) + u32(track_id) + u32(0) + u32(duration) +
               b"\0" * 8 + u16(0) + u16(0) + u16(0) + u16(0) + matrix +
               u32(width << 16) + u32(height << 16))
    return fullbox("tkhd", 0, 3, payload)


def make_mdhd(timescale, duration=120000):
    return fullbox("mdhd", 0, 0, u32(0) + u32(0) + u32(timescale) + u32(duration) + u16(0x55C4) + u16(0))


def make_hdlr(handler, name):
    return fullbox("hdlr", 0, 0, u32(0) + handler.encode("ascii") + b"\0" * 12 + name.encode("ascii") + b"\0")


def make_avcc():
    sps = b"\x42\x00\x1e\x67\x42\x00\x1e\x8c\xd4\x04\x40"
    pps = b"\x68\xce\x3c\x80"
    payload = (bytes([1, 0x42, 0x00, 0x1E, 0xFF, 0xE1]) + u16(len(sps)) + sps +
               bytes([1]) + u16(len(pps)) + pps)
    return box("avcC", payload)


def make_hvcc():
    payload = (bytes([1, 0x01, 0x60, 0x00, 0x00, 0x00, 0x90, 0x00, 0x00, 0x00,
                      0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
                      0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01]) +
              b"\x01\x01\x60\x00" + b"\0" * 6 + b"\0" * 8)
    return box("hvcC", payload)


def make_avc1(fourcc="avc1", config=None):
    config = config or make_avcc()
    payload = (b"\0" * 6 + u16(1) + u16(0) + u16(0) + b"\0" * 12 +
               u16(1920) + u16(1080) + u32(0x00480000) + u32(0x00480000) +
               u32(0) + u16(1) + b"\0" * 32 + u16(0x0018) + u16(0xFFFF) + config)
    return box(fourcc, payload)


def make_esds():
    decoder_specific = bytes([0x04, 0x10, 0x40, 0x15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0])
    decoder_config = bytes([0x04, 0x12, 0x40, 0x15]) + u24(48000) + u32(96000) + decoder_specific
    payload = bytes([0x03, len(decoder_config) + 3, 0x00, 0x01]) + u32(0) + u32(0) + decoder_config
    return fullbox("esds", 0, 0, payload)


def make_mp4a():
    payload = (b"\0" * 6 + u16(1) + b"\0" * 8 + u16(2) + u16(16) + u16(0) + u16(0) +
               u32(AUDIO_TIMESCALE << 16) + make_esds())
    return box("mp4a", payload)


def make_stsd(entry):
    return fullbox("stsd", 0, 0, u32(1) + entry)


def make_trak(track_id, handler, codec_entry, timescale, width=0, height=0):
    stbl = box("stbl", make_stsd(codec_entry))
    media_header = (box("vmhd", bytes([0, 0, 0, 1]) + b"\0" * 8) if handler == "vide"
                    else box("smhd", bytes([0, 0, 0, 0]) + b"\0" * 4))
    minf = box("minf", media_header + stbl)
    mdia = box("mdia", make_mdhd(timescale) + make_hdlr(handler, "fixture") + minf)
    return box("trak", make_tkhd(track_id, width=width, height=height) + mdia)


def make_trex(track_id, duration, size):
    return fullbox("trex", 0, 0, u32(track_id) + u32(1) + u32(duration) + u32(size) + u32(0))


def make_init(video_config=None, video_timescale=VIDEO_TIMESCALE, video_entry="avc1",
              use_box64=False, prefix=b""):
    video = make_avc1(fourcc=video_entry, config=video_config) if video_entry != "hvc1" else \
        make_avc1(fourcc="hvc1", config=video_config or make_hvcc())
    moov = box("moov", make_mvhd() +
               make_trak(1, "vide", video, video_timescale, 1920, 1080) +
               make_trak(2, "soun", make_mp4a(), AUDIO_TIMESCALE) +
               box("mvex", make_trex(1, 3000, 100) + make_trex(2, AUDIO_SAMPLE_DURATION, 50)))
    return prefix + (box64("ftyp", make_ftyp()[8:]) if use_box64 else make_ftyp()) + moov


def make_tfhd(track_id, default_duration=None):
    flags = 0x020000
    payload = u32(track_id)
    if default_duration is not None:
        flags |= 0x000008
        payload += u32(default_duration)
    return fullbox("tfhd", 0, flags, payload)


def make_tfdt(version, value):
    return fullbox("tfdt", version, 0, u32(value) if version == 0 else u64(value))


def make_trun(version, entries, has_duration):
    flags = 0x000001 | 0x000200 | 0x000800
    if has_duration:
        flags |= 0x000100
    payload = u32(len(entries)) + i32(0)
    for entry in entries:
        if has_duration:
            payload += u32(entry["duration"])
        payload += u32(entry["size"])
        payload += u32(entry["cto"]) if version == 0 else i32(entry["cto"])
    return fullbox("trun", version, flags, payload)


def tfdt_version(sequence):
    return 0 if sequence in (1, 4, 5, 6, 9, 10, 11, 12) else 1


def make_segment(sequence):
    video_dts = VIDEO_START + (sequence - 1) * VIDEO_SEGMENT_DURATION
    audio_dts = AUDIO_START + (sequence - 1) * AUDIO_DTS_STEP
    video_version = tfdt_version(sequence)
    audio_version = tfdt_version(sequence)
    video_raw = video_dts & 0xFFFFFFFF if video_version == 0 else video_dts
    audio_raw = audio_dts & 0xFFFFFFFF if audio_version == 0 else audio_dts
    video_entries = [{"duration": 3000, "size": 100, "cto": cto} for cto in (0, 3000)]
    audio_entries = [{"size": 50, "cto": -512 if i == 0 else 0} for i in range(AUDIO_SAMPLES)]

    mfhd = fullbox("mfhd", 0, 0, u32(sequence))
    video_track = make_tfhd(1, 3000) + make_tfdt(video_version, video_raw) + make_trun(0, video_entries, True)
    audio_track = make_tfhd(2, AUDIO_SAMPLE_DURATION if sequence % 2 == 1 else None) + \
        make_tfdt(audio_version, audio_raw) + make_trun(1, audio_entries, False)
    traf_video = box("traf", video_track)
    traf_audio = box("traf", audio_track)
    moof = box("moof", mfhd + traf_video + traf_audio)

    video_payload = bytes((i * 7 + sequence * 13) & 0xFF for i in range(200))
    audio_payload = bytes((i * 3 + sequence * 17) & 0xFF for i in range(AUDIO_SAMPLES * 50))
    mdat = box("mdat", video_payload + audio_payload)
    moof_size = len(moof)
    video_trun_start = (8 + len(mfhd) + 8 + len(make_tfhd(1, 3000)) +
                        len(make_tfdt(video_version, video_raw)))
    audio_trun_start = (8 + len(mfhd) + len(traf_video) + 8 +
                        len(make_tfhd(2, AUDIO_SAMPLE_DURATION if sequence % 2 == 1 else None)) +
                        len(make_tfdt(audio_version, audio_raw)))
    patched = bytearray(moof)
    struct.pack_into(">I", patched, video_trun_start + 16, moof_size + 8)
    struct.pack_into(">I", patched, audio_trun_start + 16, moof_size + 8 + len(video_payload))
    return bytes(patched) + mdat


def write_file(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as output:
        output.write(data)


def generate(out_dir):
    init = make_init()
    drone_dir = os.path.join(out_dir, "drone_a")
    write_file(os.path.join(drone_dir, "init.mp4"), init)
    filenames = {1: "seg_0001.m4s", 2: "seg_0002.m4s", 4: "seg_0003.m4s", 3: "seg_0004.m4s",
                 5: "seg_0005.m4s", 6: "seg_0006.m4s", 8: "seg_0008.m4s", 9: "seg_0009.m4s",
                 10: "seg_0010.m4s", 11: "seg_0011.m4s", 12: "seg_0012.m4s"}
    for sequence, name in filenames.items():
        write_file(os.path.join(drone_dir, name), make_segment(sequence))
    write_file(os.path.join(drone_dir, "seg_0005_repeat.m4s"), make_segment(5))

    changed_avcc = bytearray(make_avcc())
    changed_avcc[12] ^= 0x01
    incompatible = make_init(video_config=bytes(changed_avcc), video_timescale=90001)
    write_file(os.path.join(out_dir, "incompatible_init.mp4"), incompatible)

    hvc_dir = os.path.join(out_dir, "drone_hvc")
    write_file(os.path.join(hvc_dir, "init.mp4"), make_init(video_entry="hvc1"))
    write_file(os.path.join(hvc_dir, "seg_0001.m4s"), make_segment(1))

    uuid = bytes(range(16))
    uuid_box = u32(32) + b"uuid" + uuid + b"boundary"
    box64_dir = os.path.join(out_dir, "drone_box64")
    write_file(os.path.join(box64_dir, "init.mp4"), make_init(use_box64=True, prefix=uuid_box))
    write_file(os.path.join(box64_dir, "seg_0001.m4s"), make_segment(1))

    malformed_dir = os.path.join(out_dir, "malformed")
    write_file(os.path.join(malformed_dir, "init.mp4"), init)
    write_file(os.path.join(malformed_dir, "seg_0001.m4s"), make_segment(1)[:20])
    zero_dir = os.path.join(out_dir, "malformed_size_zero")
    write_file(os.path.join(zero_dir, "init.mp4"), init)
    write_file(os.path.join(zero_dir, "seg_0001.m4s"), b"\0\0\0\0moof" + b"\0" * 16)
    uuid_dir = os.path.join(out_dir, "malformed_uuid")
    write_file(os.path.join(uuid_dir, "init.mp4"), init)
    write_file(os.path.join(uuid_dir, "seg_0001.m4s"), u32(24) + b"uuid" + b"\0\0\0\0")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", required=True, help="fixture output directory")
    args = parser.parse_args()
    generate(args.out)
    print(f"fixtures generated at {args.out}")


if __name__ == "__main__":
    main()
