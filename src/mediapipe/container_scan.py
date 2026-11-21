"""MP4/MOV 容器级预检：在交给 PyAV 之前做一次纯字节 box 扫描。

PyAV/FFmpeg 报错时往往只给“无效数据”而无字节位置。本模块按 ISO-BMFF
结构遍历顶层与嵌套 box，能够：
* 发现声明 size 超出文件尾的 box —— 精确定位首个越界 box 头；
* 发现缺失的 moov（索引）/ftyp（品牌）—— 给出对应区间；
* 给出 mdat 负载范围，供坏包类损坏界定保守偏移区间。

无法确定到单个字节时一律返回半开区间，不伪称精确。
"""
from __future__ import annotations

import os
from dataclasses import dataclass

_BOX_HEADER = 8
_FULLBOX_HEADER = 12


@dataclass(frozen=True, slots=True)
class Box:
    type: bytes
    start: int
    end: int

    @property
    def header_end(self) -> int:
        return self.start + _BOX_HEADER


def scan_top_boxes(data: bytes) -> tuple[list[Box], int | None]:
    """扫描顶层 box；返回 (box 列表, 首个越界 box 头偏移或 None)。"""
    boxes: list[Box] = []
    pos = 0
    first_oob: int | None = None
    n = len(data)
    while pos + _BOX_HEADER <= n:
        size = int.from_bytes(data[pos:pos + 4], "big")
        btype = data[pos + 4:pos + 8]
        if size == 0:  # box 延伸到文件尾
            size = n - pos
        if size == 1:  # 64 位 largesize
            if pos + 16 > n:
                first_oob = pos if first_oob is None else first_oob
                break
            size = int.from_bytes(data[pos + 8:pos + 16], "big")
        if size < _BOX_HEADER or pos + size > n:
            first_oob = pos if first_oob is None else first_oob
            break
        boxes.append(Box(type=btype, start=pos, end=pos + size))
        pos += size
    return boxes, first_oob


def precheck_container(data: bytes) -> dict[str, object]:
    """返回预检结论：是否结构完整、缺失 box、越界偏移与 mdat 范围。"""
    boxes, first_oob = scan_top_boxes(data)
    types = [b.type for b in boxes]
    mdat = next((b for b in boxes if b.type == b"mdat"), None)
    result: dict[str, object] = {
        "intact": first_oob is None,
        "first_oob_offset": first_oob,
        "has_ftyp": b"ftyp" in types,
        "has_moov": b"moov" in types,
        "has_mdat": mdat is not None,
        "box_types": [t.decode("latin1") for t in types],
        "mdat_payload_start": (mdat.header_end if mdat else None),
        "mdat_end": (mdat.end if mdat else None),
        "file_size": len(data),
    }
    return result


def precheck_file(path: str) -> dict[str, object]:
    """流式预检：只 seek + 读 box 头（8/16 字节），不把文件载入内存。"""
    file_size = os.path.getsize(path)
    boxes: list[Box] = []
    first_oob: int | None = None
    with open(path, "rb") as fh:
        pos = 0
        while pos + _BOX_HEADER <= file_size:
            fh.seek(pos)
            header = fh.read(_BOX_HEADER)
            if len(header) < _BOX_HEADER:
                break
            size = int.from_bytes(header[:4], "big")
            btype = header[4:8]
            if size == 0:
                size = file_size - pos
            elif size == 1:
                ext = fh.read(8)
                if len(ext) < 8:
                    first_oob = pos if first_oob is None else first_oob
                    break
                size = int.from_bytes(ext, "big")
            if size < _BOX_HEADER or pos + size > file_size:
                first_oob = pos if first_oob is None else first_oob
                break
            boxes.append(Box(type=btype, start=pos, end=pos + size))
            pos += size
    types = [b.type for b in boxes]
    mdat = next((b for b in boxes if b.type == b"mdat"), None)
    moov = next((b for b in boxes if b.type == b"moov"), None)
    declared = None
    if moov is not None:
        with open(path, "rb") as fh:
            fh.seek(moov.start)
            moov_bytes = fh.read(moov.end - moov.start)
        declared = _sum_stsz_samples(moov_bytes)
    return {
        "intact": first_oob is None,
        "first_oob_offset": first_oob,
        "has_ftyp": b"ftyp" in types,
        "has_moov": b"moov" in types,
        "has_mdat": mdat is not None,
        "box_types": [t.decode("latin1") for t in types],
        "mdat_payload_start": (mdat.header_end if mdat else None),
        "mdat_end": (mdat.end if mdat else None),
        "file_size": file_size,
        "declared_sample_count": declared,
    }


def _sum_stsz_samples(moov_bytes: bytes) -> int | None:
    """在 moov 字节内汇总所有 stsz 声明的样本总数（moov 体积小，可整体读）。"""
    total = 0
    found = False
    idx = 0
    needle = b"stsz"
    while True:
        j = moov_bytes.find(needle, idx)
        if j < 0:
            break
        # j 指向 type 字段；sample_count 在 type 后 12 字节（box 头 8 + ver/flags 4 + size 4）
        count_off = j + 12
        if count_off + 4 <= len(moov_bytes):
            total += int.from_bytes(
                moov_bytes[count_off:count_off + 4], "big"
            )
            found = True
        idx = j + 4
    return total if found else None
