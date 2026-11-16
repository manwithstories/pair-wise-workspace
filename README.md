# mkv2mp4

把游戏录制/离线媒体工具链导出的 Matroska 文件转封装为可供剪辑、归档与
点播系统读取的 MP4，并给出可审计的轨道映射与时间戳重写依据。

纯本地运行：Python 3.11+ 与 NumPy（用于时间戳数组运算），不调用 ffmpeg、
mkvmerge 等外部媒体工具，也不访问网络。

## 快速开始

```bash
python -m src.cli inspect --generate-fixture --json
python -m src.cli remux   --generate-fixture --output /tmp/sample.mp4 --json
python -m src.cli verify  --input /tmp/sample.mp4 --json
```

`--generate-fixture` 现场生成演示样本（未知长度 Segment/Cluster、负值/重复/
回退时间戳、含 Cues），`--fixture-path` 指定落点（默认
`outputs/fixture.mkv`）。读取真实文件用 `--input <file>` 或 `--stdin`。

不加 `--json` 时输出终端摘要，数字与 JSON 完全一致。

## 三个子命令

| 子命令 | 作用 |
|---|---|
| `inspect` | 元素摘要（元素数、未知长度数、Cluster/Cue/附件）、轨道表、时间戳重写计划、轨道映射 |
| `remux` | 生成 MP4，报告轨道映射、样本数、重写前后首末时间戳、box 数与偏移、sha256 |
| `verify` | 独立回读 MP4：box 边界/连续性、mdat 覆盖、逐轨表一致性、索引顺序、哈希 |

`remux --verify` 可在写入后立即复核；`verify --source <mkv>` 追加源侧字节
体量覆盖核对，`--expect-sha256` 校验摘要。

## 退出码

| 码 | 含义 |
|---|---|
| 0 | 全部通过 |
| 2 | 输入或参数错误 |
| 3 | EBML/MP4 结构损坏（含不支持的 CodecID） |
| 4 | 时间戳或索引无法重建 |
| 5 | 输出校验失败 |

## 关键行为

- **时间戳**：按 TimecodeScale 统一到 MP4 timescale（视频 90 kHz、音频
  48 kHz、字幕 1 kHz）。文档级共用一次平移，负值整体移到非负且不破坏
  音画相对关系；重复值按轨道最小合法步长前推；回退值视为 B 帧呈现顺序，
  保留原 PTS 并在 `ctts` 中表达（`--force-monotonic-pts` 可强制压平）。
- **轨道映射**：以 TrackUID 为稳定身份，保留视频/音频/字幕的 CodecID、
  语言、默认/强制标记与原始编号；缺 TrackUID 时用属性哈希派生并报告
  `uid_source=hash` 与降级原因。
- **采样表**：stsz/stts/ctts/stsc/stco（>4 GiB 自动 co64）/stss；交错
  Cluster、跨 Cluster 样本、空 Cluster、缺失 Cues、无关键帧视频均可处理。
- **只转封装不重编码**：样本按源字节区间复制；遇到不支持的 CodecID、损坏
  Block 或无法推断的样本时停止写入并删除不完整输出（`--lenient` /
  `--allow-unsupported-codecs` 显式降级为跳过并计数告警）。写入后每个样本
  按其偏移回读比对，确保没有错位。
- **容错模式**（`--lenient`）：未知 EBML 元素记为告警而非失败；输出附带
  `warning_count`。

## 测试

```bash
python -m unittest discover -s tests     # 单元 + 端到端
python scripts/acceptance.py             # 三条验收命令的契约断言
```

## 模块

```
src/ebml_schema.py       元素 ID 表、VINT 与载荷编解码
src/ebml_reader.py       流式解析（未知长度容器、层级、绝对偏移）
src/matroska_model.py    Segment/Info/Tracks/Cluster/Cues/Attachment 模型
src/timestamp.py         TimecodeScale 统一与重写计划
src/track_mapper.py      TrackUID 稳定映射与降级报告
src/mp4_boxes.py         box 构造、序列化与回读解析
src/index_rebuilder.py   采样表、关键帧、解码/合成时间表
src/fixture_builder.py   程序化 Matroska 夹具
src/cli.py               inspect / remux / verify
```
