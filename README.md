# mkvkit

纯 Python 的 Matroska（MKV）到 ISO-BMFF（MP4）转封装工具，面向庭审录播、
远程调解存证等需要固定轨道映射和可核验时间戳的场景。工具不重编码、不调用
`ffmpeg`/`ffprobe` 或第三方解封装库，只读取输入文件并写出用户指定的 MP4。

## 安装与测试

```bash
python -m pip install -e .
python tests/make_fixtures.py --out tests/fixtures --seed 7
python -m pytest -q
```

运行环境为 Python 3.11+、NumPy 1.26.4；测试依赖 pytest 8.2.0。夹具全部由
`tests/make_fixtures.py` 用标准库生成，不依赖外部媒体文件。

## 命令

```bash
# 摘要路径：完整树供汇总，JSON 仅导出指定深度
python -m mkvkit.cli inspect tests/fixtures/unknown_len.mkv \
  --depth 4 --json out/tree.json

# 转封装并生成交接报告
python -m mkvkit.cli remux tests/fixtures/unknown_len.mkv \
  -o out/unknown_len.mp4 --report out/report.json

# 只读回验 MP4
python -m mkvkit.cli verify out/unknown_len.mp4 --json
```

`inspect` 的汇总始终统计完整元素树；`--depth` 只限制 JSON 中 `elements`
数组的导出深度。未知长度 Segment/Cluster 以 EOF 或下一个同级元素定界。

## 轨道与时间戳

- `V_MPEG4/ISO/AVC` -> `avc1`，CodecPrivate 原样写入 `avcC`。
- `A_AAC*` -> `mp4a`，CodecPrivate 写入 `esds`。
- `S_TEXT/UTF8` -> `tx3g`，样本增加 tx3g 的 16 位长度前缀。
- 视频轨道使用 90 kHz，音频轨道使用采样率，字幕轨道使用 1 kHz 时基。
- lacing 帧时优先使用 BlockDuration，其次 DefaultDuration；缺省 AAC 时按
  1024 样本/帧推算。Matroska PTS 按解码顺序重建严格递增 DTS，组合偏移写入
  `ctts`，负偏移通过 `elst` 平移；轨间正的首帧偏移通过 `elst` 空编辑保留。
- 重复或严重倒退时间戳会修复并计数；无法修复时退出码为 4。

## 交接报告

`--report` 输出 JSON，包含每轨 `track/codec/timescale/frames/first_pts_ns/
last_pts_ns`、`av_start_delta_ms`、`co64_used`、`issues` 与问题明细。
`verify --json` 另给出 `dts_monotonic`、`ctts_negative_count`、
`elst_media_time`、`stco_entries`、`mdat_bounds_ok` 和逐轨样本数。

100MB 级单文件在本机烟雾测试中 remux 约 0.3 秒、峰值常驻内存约 146MB；
实际值取决于轨道数、帧数和磁盘速度。

## 退出码

| 码 | 含义 |
|---:|---|
| 0 | 成功 |
| 1 | 参数/输出路径错误 |
| 2 | 非 Matroska、EBML 截断或无法解析 |
| 3 | 编码不受支持或 CodecPrivate 缺失 |
| 4 | 时间戳无法修复 |
| 5 | 写出后自检失败或 verify 未通过 |

## 结构

```text
mkvkit/ebml/       VINT、静态 schema、流式 reader、元素树与摘要
mkvkit/mkv/        Segment/Tracks/Cluster/BlockGroup 与三种 lacing
mkvkit/timing/     PTS/DTS、ctts、elst、时基换算与 NumPy 校验
mkvkit/isobmff/    box 序列化、moov/mdat、偏移回填与只读回验
mkvkit/cli.py      inspect/remux/verify 与退出码
tests/             纯 Python 夹具生成器、单元与端到端测试
```
