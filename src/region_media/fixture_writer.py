"""本地样例夹具生成（下）：目录落盘与 HLS/DASH 清单写入。

目录布局（每个场景独立子目录）::

    <out>/<scenario>/
        master.m3u8          HLS 主播放列表（音频+视频媒体列表）
        audio.m3u8 / video.m3u8
        stream.mpd           同内容 DASH 表示（SegmentTemplate）
        seg_audio_<n>.mka / seg_video_<n>.mkv
        meta.json            场景参数（供 verify 断言）

主目录 ``<out>/master.m3u8`` 指向 normal 场景，满足验收命令的固定路径。
"""

from __future__ import annotations

import json
import os

from .generator import (
    FPS_NOMINAL,
    SCENARIOS,
    SAMPLE_RATE,
    SEG_SECONDS,
    ScenarioPlan,
    SegmentSpec,
    build_plan,
    write_segment_file,
)
from .model import TrackKind

DASH_TIMESCALE = 90000


def _write_hls(root: str, plan: ScenarioPlan,
               audio_uris: list[str], video_uris: list[str],
               truncated: bool) -> None:
    dur = f"{SEG_SECONDS:.3f}"
    audio_lines = ["#EXTM3U", "#EXT-X-VERSION:6",
                   "#EXT-X-TARGETDURATION:1",
                   "#EXT-X-MEDIA-SEQUENCE:0"]
    for uri in audio_uris:
        audio_lines += [f"#EXTINF:{dur},", uri]
    if not truncated:
        audio_lines.append("#EXT-X-ENDLIST")
    with open(os.path.join(root, "audio.m3u8"), "w", encoding="utf-8") as fh:
        fh.write("\n".join(audio_lines) + "\n")

    video_lines = ["#EXTM3U", "#EXT-X-VERSION:6",
                   "#EXT-X-TARGETDURATION:1",
                   "#EXT-X-MEDIA-SEQUENCE:0"]
    for uri in video_uris:
        video_lines += [f"#EXTINF:{dur},", uri]
    if not truncated:
        video_lines.append("#EXT-X-ENDLIST")
    with open(os.path.join(root, "video.m3u8"), "w", encoding="utf-8") as fh:
        fh.write("\n".join(video_lines) + "\n")

    master = [
        "#EXTM3U", "#EXT-X-VERSION:6",
        '#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="en",'
        'URI="audio.m3u8"',
        '#EXT-X-STREAM-INF:BANDWIDTH=400000,AUDIO="aud"',
        "video.m3u8",
    ]
    with open(os.path.join(root, "master.m3u8"), "w", encoding="utf-8") as fh:
        fh.write("\n".join(master) + "\n")


def _write_dash(root: str, plan: ScenarioPlan, truncated: bool) -> None:
    n = plan.segments
    duration_ticks = int(SEG_SECONDS * DASH_TIMESCALE)
    s_entries = "".join(
        f'<S d="{duration_ticks}"/>' for _ in range(n - (1 if truncated else 0)))
    mpd_type = "dynamic" if truncated else "static"
    mpd = f"""<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="{mpd_type}"
     mediaPresentationDuration="PT{SEG_SECONDS * n:.1f}S"
     minBufferTime="PT0.2S">
  <Period>
    <AdaptationSet contentType="audio" segmentAlignment="true">
      <SegmentTemplate timescale="{SAMPLE_RATE}" startNumber="0"
        media="seg_audio_$Number$.mka">
        <SegmentTimeline>{s_entries}</SegmentTimeline>
      </SegmentTemplate>
      <Representation id="audio0" mimeType="audio/x-matroska"
                      audioSamplingRate="{SAMPLE_RATE}"/>
    </AdaptationSet>
    <AdaptationSet contentType="video" segmentAlignment="true">
      <SegmentTemplate timescale="{DASH_TIMESCALE}" startNumber="0"
        media="seg_video_$Number$.mkv">
        <SegmentTimeline>{s_entries}</SegmentTimeline>
      </SegmentTemplate>
      <Representation id="video0" mimeType="video/x-matroska"
                      width="160" height="120" frameRate="{FPS_NOMINAL}"/>
    </AdaptationSet>
  </Period>
</MPD>
"""
    with open(os.path.join(root, "stream.mpd"), "w", encoding="utf-8") as fh:
        fh.write(mpd)


def generate_scenario(root: str, plan: ScenarioPlan) -> dict:
    """生成单个场景的全部媒体分片与两套清单，返回 meta 字典。"""
    os.makedirs(root, exist_ok=True)
    audio_uris, video_uris = [], []
    truncated = bool(plan.extra.get("truncate_manifest"))
    written = plan.segments - (1 if truncated else 0)
    for seq in range(plan.segments):
        a_uri = f"seg_audio_{seq}.mka"
        v_uri = f"seg_video_{seq}.mkv"
        if seq < written:
            corrupt = plan.corrupt_seg == seq
            write_segment_file(os.path.join(root, a_uri), SegmentSpec(
                seq=seq, kind=TrackKind.AUDIO, uri=a_uri,
                scenario=plan.name, seed=plan.seed + seq,
                vfr=plan.vfr, corrupt=corrupt))
            write_segment_file(os.path.join(root, v_uri), SegmentSpec(
                seq=seq, kind=TrackKind.VIDEO, uri=v_uri,
                scenario=plan.name, seed=plan.seed + seq,
                vfr=plan.vfr, corrupt=corrupt))
            audio_uris.append(a_uri)
            video_uris.append(v_uri)
    _write_hls(root, plan, audio_uris, video_uris, truncated)
    _write_dash(root, plan, truncated)
    meta = {"scenario": plan.name, "segments": plan.segments,
            "written": written, "truncated": truncated,
            "vfr": plan.vfr, "corrupt_seg": plan.corrupt_seg,
            "profile": plan.extra, "seed": plan.seed}
    with open(os.path.join(root, "meta.json"), "w", encoding="utf-8") as fh:
        json.dump(meta, fh, ensure_ascii=False, indent=2)
    return meta


def generate_all(out_dir: str, seed: int = 13812) -> dict[str, dict]:
    """生成全部场景；根 master.m3u8 固定指向 normal 场景。"""
    out_dir = os.path.abspath(out_dir)
    os.makedirs(out_dir, exist_ok=True)
    metas: dict[str, dict] = {}
    for name in SCENARIOS:
        metas[name] = generate_scenario(
            os.path.join(out_dir, name), build_plan(name, seed))
    # 验收固定入口：根 master.m3u8 引用 normal 子目录的媒体列表
    with open(os.path.join(out_dir, "master.m3u8"), "w",
              encoding="utf-8") as fh:
        fh.write("#EXTM3U\n#EXT-X-VERSION:6\n"
                 '#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="en",'
                 'URI="normal/audio.m3u8"\n'
                 '#EXT-X-STREAM-INF:BANDWIDTH=400000,AUDIO="aud"\n'
                 "normal/video.m3u8\n")
    with open(os.path.join(out_dir, "index.json"), "w",
              encoding="utf-8") as fh:
        json.dump(metas, fh, ensure_ascii=False, indent=2)
    return metas
