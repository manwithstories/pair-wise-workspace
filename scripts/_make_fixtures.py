#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""生成可复现的 PCM/WAV 验收样例（固定种子 12785，仅用标准库）。

输出到目标目录：
  normal_6ch.wav(+idx)       48 kHz 六声道 3 s 正常样例
  truncated_6ch.wav(+idx)    尾部截断 7 字节，头部仍声明原始长度
  damaged_6ch.wav(+idx)      两个 0.1 s 索引页内各翻转 1 字节
  missing_index_6ch.wav      不写索引侧车，触发顺序扫描重建
所有随机内容由固定种子的线性同余生成器产生，重复运行字节一致。
"""
import argparse
import json
import os
import struct
import zlib

SEED = 12785
RATE = 48000
CHANNELS = 6
BPS = 16
BLOCK = CHANNELS * BPS // 8          # 12 字节/采样帧
FRAMES_PER_PAGE = 4800               # 0.1 s 一页
DAMAGED_PAGES = (4, 17)              # 损坏样例中翻转字节的页号（共 2 页）
TRUNCATE_BYTES = 7


def pcm_bytes(frames):
    """确定性 s16 样本，一次 struct 打包，避免逐值 Python 循环。"""
    state = SEED & 0xFFFFFFFF
    values = []
    total = frames * CHANNELS
    for _ in range(total):
        state = (state * 1103515245 + 12345) & 0xFFFFFFFF
        values.append((state >> 8) & 0x7FFF)
    return struct.pack("<%dH" % total, *values)


def wav_bytes(pcm):
    header = b"RIFF" + struct.pack("<I", 36 + len(pcm)) + b"WAVE"
    header += b"fmt " + struct.pack("<IHHIIHH", 16, 1, CHANNELS, RATE,
                                    RATE * BLOCK, BLOCK, BPS)
    header += b"data" + struct.pack("<I", len(pcm))
    return header + pcm


def index_pages(pcm, available_bytes):
    """对磁盘上的完整帧做单次顺序切分并逐页计算 CRC32。"""
    usable = (available_bytes // BLOCK) * BLOCK
    pages = []
    offset = 44
    sample = 0
    page_no = 0
    view = memoryview(pcm)
    while offset - 44 < usable:
        start = offset - 44
        size = min(FRAMES_PER_PAGE * BLOCK, usable - start)
        frames = size // BLOCK
        size = frames * BLOCK
        pages.append({
            "page_no": page_no,
            "offset": offset,
            "size": size,
            "start_sample": sample,
            "end_sample": sample + frames,
            "crc32": zlib.crc32(view[start:start + size]) & 0xFFFFFFFF,
        })
        offset += size
        sample += frames
        page_no += 1
    return pages


def write_index(path, pages):
    with open(path + ".idx.json", "w", encoding="utf-8") as f:
        json.dump({"pages": pages}, f, indent=2)
        f.write("\n")


def write_wav(path, blob):
    with open(path, "wb") as f:
        f.write(blob)


def main():
    ap = argparse.ArgumentParser(description="生成 mediaqc 验收样例")
    ap.add_argument("outdir", help="输出目录（不存在则创建）")
    args = ap.parse_args()
    os.makedirs(args.outdir, exist_ok=True)

    pcm3 = pcm_bytes(RATE * 3)
    pcm2 = pcm_bytes(RATE * 2)

    # 1) 正常样例：3 s。
    normal = os.path.join(args.outdir, "normal_6ch.wav")
    write_wav(normal, wav_bytes(pcm3))
    write_index(normal, index_pages(pcm3, len(pcm3)))

    # 2) 截断样例：3 s 内容物理截短 7 字节，头部仍声明原始长度。
    trunc = os.path.join(args.outdir, "truncated_6ch.wav")
    write_wav(trunc, wav_bytes(pcm3)[:-TRUNCATE_BYTES])
    write_index(trunc, index_pages(pcm3, len(pcm3) - TRUNCATE_BYTES))

    # 3) 损坏样例：3 s，先按干净内容建索引，再翻转两页内各 1 字节。
    damaged = os.path.join(args.outdir, "damaged_6ch.wav")
    write_wav(damaged, wav_bytes(pcm3))
    write_index(damaged, index_pages(pcm3, len(pcm3)))
    with open(damaged, "r+b") as f:
        for page in DAMAGED_PAGES:
            f.seek(44 + page * FRAMES_PER_PAGE * BLOCK + 3)
            b = f.read(1)
            f.seek(-1, os.SEEK_CUR)
            f.write(bytes([b[0] ^ 0xFF]))

    # 4) 索引缺页样例：2 s，不写 .idx.json，由扫描器顺序重建。
    missing = os.path.join(args.outdir, "missing_index_6ch.wav")
    write_wav(missing, wav_bytes(pcm2))

    params = {
        "seed": SEED,
        "rate_hz": RATE,
        "channels": CHANNELS,
        "bits_per_sample": BPS,
        "frames_per_index_page": FRAMES_PER_PAGE,
        "truncate_bytes": TRUNCATE_BYTES,
        "damaged_pages": list(DAMAGED_PAGES),
        "fixtures": [
            "normal_6ch.wav",
            "truncated_6ch.wav",
            "damaged_6ch.wav",
            "missing_index_6ch.wav",
        ],
    }
    with open(os.path.join(args.outdir, "fixture-params.json"), "w", encoding="utf-8") as f:
        json.dump(params, f, indent=2, ensure_ascii=False)
        f.write("\n")
    print("fixtures written to", args.outdir)
    for name in ("normal_6ch.wav", "truncated_6ch.wav",
                 "damaged_6ch.wav", "missing_index_6ch.wav"):
        p = os.path.join(args.outdir, name)
        print(f"  {name}: {os.path.getsize(p)} bytes")


if __name__ == "__main__":
    main()
