# mkvtomp4

纯 Python/NumPy 的 Matroska 转封装器，面向在线课程归档和版权取证。它不调用
ffmpeg、mkvmerge 或其他外部二进制，所有样例均由固定种子生成。

## 快速验收

```bash
python fixtures/gen_fixtures.py --out fixtures --seed 7
python -m mkvtomp4.cli probe fixtures/live_unknown.mkv --json
python -m mkvtomp4.cli remux fixtures/live_unknown.mkv --path plain --out out/plain.mp4 --json
python -m mkvtomp4.cli remux fixtures/live_unknown.mkv --path frag --out out/frag.mp4 --frag-duration 2 --json
python -m mkvtomp4.cli verify out/plain.mp4 --source fixtures/live_unknown.mkv --json
python -m mkvtomp4.cli verify out/frag.mp4 --source fixtures/live_unknown.mkv --json
python -m mkvtomp4.cli selftest --fixtures fixtures
```

所有命令都向 stdout 输出一个 JSON 对象。`probe` 展示元素数量、最大深度、各 ID
计数、未知长度元素数、Cues 条目、Segment 数以及前 20 个 Segment 顶层元素。
`remux` 展示轨道映射、时间戳统计、索引来源与产物信息。`verify` 回读 MP4 并与
归一化后的源时间轴逐轨比较。

## 设计

- `mkvtomp4/ebml/`：1-8 字节 VINT、流式游标、元素 schema、未知长度与 CRC-32。
- `mkvtomp4/mkv/`：元素树、轨道映射、Cluster/Block 扫描、Cues 边界校验。
- `mkvtomp4/timing/`：NumPy 批量毫秒换算、全局负偏移、重复 DTS 丢弃、PTS 重排。
- `mkvtomp4/mp4/`：ISO BMFF box、stts/stss/stco/stsz、普通与分片导出。
- `mkvtomp4/mp4/readback.py` 与 `mkvtomp4/verify.py`：从 moov/moof/trun 回读样本数、
  首末时间戳和关键帧数，并与源时间轴比对。
- `fixtures/gen_fixtures.py`：生成 live 断线、无 Cues、坏 Cues、负 DTS、重复 DTS、
  多 Segment、截断、坏 CRC、缺轨道和零样本夹具。

两条导出路径都只接收一次解析、一次归一化得到的 `NormalizedMedia`；导出器不再
解析 Matroska。普通路径输出 `ftyp + moov + mdat`，分片路径输出
`ftyp + moov(empty tables + mvex) + (moof + mdat)*`，视频在关键帧处分片。

## 退出码

| 退出码 | 含义 |
| --- | --- |
| 0 | 成功 |
| 2 | EBML/文件结构非法，或无法安全续接的多 Segment |
| 3 | 无可用轨道，或映射轨道零样本 |
| 4 | 时间戳无法单调化 |
| 5 | 回读解析失败或与源偏差超过一个 timescale 单位 |

导出采用同目录临时文件加原子替换；失败时清理临时文件。`__pycache__/` 与 `out/`
已加入 `.gitignore`。
