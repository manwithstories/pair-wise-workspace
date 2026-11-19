# media-restore

影视资料馆的介资恢复工具：把旧磁带数字化得到的 H.264/H.265/AV1 裸码流、
带版本号的关键帧索引和不一致的音频，校验后恢复为可长期保存的档案。

纯本地运行，无外部服务；目标平台 macOS（Apple Silicon 与 Intel）。

## 构建

依赖：`brew install cmake pkg-config ffmpeg catch2`

```sh
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build
ctest --test-dir build --output-on-failure
```

## 验收流程

```sh
./build/media-restore generate --out /tmp/archive-fixtures --include-damaged
./build/media-restore restore  --manifest /tmp/archive-fixtures/archive.json \
    --rate 48000 --layout 5.1 --loudness -23 --json /tmp/archive-result.json
./build/media-restore verify   --manifest /tmp/archive-fixtures/archive.json \
    --result /tmp/archive-result.json
```

`generate` 习惯上也可通过 `scripts/make_fixtures.sh` 调用；它与 CLI 共用同一
编码路径，样例数据全部由程序生成，不引用外部文件。

终端输出包含：

- `manifest_roundtrip=100%` — 清单解析后重新序列化与原文件逐字节一致
- `keyframe_hits=N/M` — 索引条目命中扫描出的关键帧（仅健康片段计入比率）
- `segment_hash_match=N/M` — 恢复产物的 SHA-256 与结果文档一致
- `audio_rate_out` / `layout_out` / `loudness_lufs` — 音频交付参数

退出码：`0` 全部核验通过，`1` 输入或参数错误，`2` 素材校验失败，
`3` 恢复或依赖图失败。

## 子命令

| 命令 | 作用 |
| --- | --- |
| `generate --out DIR [--include-damaged]` | 生成确定性样例档案（码流、索引、音频、清单） |
| `inspect --manifest FILE [--json FILE]` | 校验清单、索引、码流内容与轨道图，不写媒体 |
| `restore ...` | 校验、按依赖图排序、恢复音频、写出哈希与结果 JSON |
| `verify --manifest FILE --result FILE` | 重新从磁盘计算哈希、索引命中与响度，不信任结果文档 |

`restore` 参数：

- `--rate` 目标采样率（8000–768000）
- `--layout` 目标声道布局（如 `mono`、`stereo`、`5.1`、`7.1`）
- `--loudness` 目标整体响度 LUFS（-70–0）
- `--true-peak` 真峰值上限 dBTP（默认 -1.0）
- `--color preserve|archive` 色彩冲突策略（默认 `archive`）
- `--archive-primaries` / `--archive-transfer` / `--archive-range` 归档色彩值，
  仅在 `--color archive` 下用于冲突或缺失字段
- `--out` 恢复产物目录（默认清单同级 `restored/`）
- `--json` 结果文档路径

## 设计要点

- **整数媒体时间**：时间一律以 `num/den` 字符串保存，内部以时间基整数刻度
  计算，128 位中间量防溢出；浮点只在展示层出现。
- **索引校验**：拒绝未知编码标记、负偏移、越界、重复、倒序与非关键帧条目；
  缺失条目由码流扫描补齐并标记 `origin=scan`。
- **损坏样例**：清单用 `quarantine.expected_code` 声明预期的媒体校验错误，
  损坏样例独立成轨，健康样例的关键帧命中率与哈希匹配率保持 100%。
- **依赖图**：Kahn 拓扑排序；环与缺失依赖定位到具体轨道，独立轨道继续，
  依赖失败的轨道记录原因且不输出伪成功结果。
- **音频**：显式采样格式与声道布局重采样，支持平面/交错输入、单声道上混到
  5.1/7.1；同速率同布局直接拷贝，避免 resampler 内部延迟造成样点漂移；
  非整帧尾部经冲刷保留；丢弃/补齐样点均计数。
- **响度**：BS.1770-4 K 加权、400 ms 门限块；静音、过短片段与不可测值走显式
  边界路径，JSON 中不出现 NaN/inf。
- **稳定哈希**：产物 SHA-256 与方法摘要哈希，重复运行逐字节一致；已有有效
  产物在重跑时复用并校验，失败片段不会覆盖。
- **资源上限**：媒体大小、索引条目、轨道/片段数、音频样点均设上限，超限以
  `E_LIMIT_EXCEEDED` 拒绝，避免整段素材无界载入内存。

## 源码结构

```
include/ + src/
  error          错误模型与退出码
  json           严格 JSON DOM 与规范化输出
  rational       有理数媒体时间（整数刻度）
  hash / sha256  稳定哈希
  file_io        限界读写、原子写入、档案路径解析
  manifest       清单模式与确定性往返
  bitstream      码流嗅探、裸流扫描、关键帧索引校验
  timeline       整数时间线、重叠诊断、依赖图排序
  audio_restore  重采样、布局归一、响度与真峰值、WAV 编解码
  color_metadata 色彩冲突记录与恢复策略
  fixture        进程内编码样例生成
  restore_engine 恢复/核验引擎与结果 JSON
tests/           Catch2 单元测试与 ctest 端到端验收
scripts/         make_fixtures.sh
```
