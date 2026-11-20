"""测试共享辅助：路径引导与轻量构造器。"""

from __future__ import annotations

import os
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(_HERE, "..", "src"))
sys.path.insert(0, os.path.join(_HERE, ".."))

from region_media.model import PacketModel, TrackKind  # noqa: E402


def pkt(seq, pts, track=TrackKind.AUDIO):
    return PacketModel(track, seq, 0, 0, pts, pts, 20_000, seq * 1000, 64)


def recovery_pkt(seq, pts, dur=20000, seg=0):
    return PacketModel(TrackKind.AUDIO, seq, seg, 0, pts, pts,
                       dur, 1000, 64)
