# media_batch

电视台后期批量素材处理工具：解析 SRT/WebVTT 与多制式时间码，按字幕与时间码
切出可复核片段，检测静音区间，按归一化音频指纹去重，分块再封装，并通过
SQLite 检查点在中断后安全恢复。全程本地运行，不联网，不修改源素材。

## 安装

macOS 终端执行：

```bash
brew install ffmpeg libsndfile
python3.12 -m venv .venv
.venv/bin/python -m pip install -e '.[test]'
```

PyAV 12.x 通过 Homebrew 提供的 FFmpeg 动态库工作；`soundfile` 0.12.x 依赖
libsndfile。第三方依赖只装进项目根目录的 `.venv`，所有命令都用
`.venv/bin/python`，不依赖系统 Python 全局包。

## 演示与验收

```bash
# 生成固定种子样例，跑到第 2 块后中断（退出码 2）
.venv/bin/python -m media_batch.cli demo --workdir /tmp/media-batch-demo --interrupt-after 2

# 从最后一个校验通过的块继续，输出 JSON（退出码 0）
.venv/bin/python -m media_batch.cli resume --workdir /tmp/media-batch-demo --json

# 校验最终输出；--corrupt-at 会先翻转一个字节再校验，并给出精确偏移（退出码 2）
.venv/bin/python -m media_batch.cli verify --workdir /tmp/media-batch-demo --corrupt-at 4096

# 单元与联调测试
.venv/bin/python -m unittest discover -s tests -v
```

### 退出码

| 码 | 含义 |
|----|------|
| 0 | 完整通过（verify 一致、resume 完成） |
| 1 | 输入存在可恢复损坏，已生成错误清单 |
| 2 | 运行被中断、恢复失败、索引损坏或输出校验失败 |

## 子命令

- `demo --workdir DIR [--interrupt-after N]`：生成样例并运行，可模拟中断。
- `run --workdir DIR [--interrupt-after N]`：在既有工作目录上运行。
- `resume --workdir DIR [--json]`：校验既有块，回滚损坏块并从其后继续。
- `verify --workdir DIR [--corrupt-at BYTE]`：校验最终 WAV；损坏时逐字节定位。

JSON 输出固定包含验收指标：

| 字段 | 含义 |
|------|------|
| `error_source_offset` | 首个错误的源文件字节偏移（-1 表示不适用）；定位误差 ≤ 1 字节 |
| `recovery_seconds` | 校验已提交检查点并确定恢复起点所用秒数（100 块内 < 3 s） |
| `reprocessed_chunks` | 本次实际重算的块序号（仅损坏块及其后未提交块） |
| `output_sha256` | 最终 WAV 的 SHA-256（64 位十六进制） |
| `subtitle_errors` | 字幕解析错误清单，含 `source`、`source_offset`、`record_index`、`code` |

另含 `duplicates`（重复块 → 代表块）、`representatives`（代表块来源区间）、
`boundary_deviations`（切点落在帧间时的实际偏差毫秒）、`silence_ms`、
`drift`（各媒体的 `ms_per_minute` 与 `trusted`）以及完整 `errors`。

## 工作目录结构

```
<workdir>/input/          样例与源素材（demo 生成，固定种子 12785）
  program.wav             48 kHz 单声道节目音频
  program.srt / .vtt      含非法时间、截断行、重叠字幕
  timecodes.txt           29.97 丢帧时间码，跨日界回绕并含重复
  timecodes_25.txt        25 fps 切点（落在 cue 内部）
  program.mp4             全长 VFR 视频 + AAC 音频
  video_only.mp4          无音轨（缺失音轨错误路径）
  truncated.mp4           字节截断（坏包错误路径）
  drift.mka               容器时钟每分钟快 180 ms（漂移路径）
  broken.mp4              不可打开（坏容器错误路径）
<workdir>/work/           中间产物
  output.pcm              逐块追加的 s16le 音频
  index.db                SQLite 检查点（每块输入校验和、输出偏移/长度、事务号）
  fingerprints.json       每块指纹与去重映射
  plan.json               切分计划
  errors.json             累积错误清单
<workdir>/output/
  program.out.wav         原子替换后的最终输出
  manifest.json           运行报告
```

## 设计要点

- **统一时间基准**（`timebase.py`）：分数帧率用 `Fraction`，音频用整数采样点，
  帧与采样的换算全部有理数运算，禁止浮点时间累积。
- **字幕解析**（`subtitle.py`）：按字节切块，保留每条记录首字节偏移；非法时间、
  截断行、缺索引行、重叠字幕记入错误清单后继续读取后续可恢复记录。
- **多制式时间码**（`timestamps.py`）：支持 24/25/30/29.97（丢帧）与日界回绕；
  重复或倒退时间戳只记录、不静默排序。
- **时钟漂移**（`timestamps.py` + `probe.py`）：用解码得到的整数采样位置对比
  容器包时间戳，按 60 秒窗口估计漂移；超过每分钟 100 ms 即标记不可信区间，
  不压缩时长补偿。
- **切分**（`pipeline.py`）：按字幕边界切分，时间码切点仅在 cue 内部生效；
  边界落在视频帧之间时记录 `snap_ms` 与实际偏差。重叠 cue 保持完整，不裁剪出
  属于后续 cue 的尾巴。
- **静音检测**（`analyze.py`）：分块 RMS（-50 dBFS，20 ms 块），跨块合并区间，
  短于 300 ms 的静音不单独输出。
- **指纹去重**（`analyze.py`）：RMS 归一、裁剪前后静音、512 个 8 kHz 以下线性
  频带的对数谱，余弦比较；不同增益与前后静音差异仍判定为同一内容。
- **检查点与恢复**（`index.py`、`pipeline.py`）：每块一个 SQLite 事务，记录输入
  校验和、输出偏移与长度、输出校验和、事务号。恢复时逐块校验，首个损坏块及其
  后全部回滚重算；索引丢失时以 PCM 实际长度为起点，绝不重复写入。
- **输出**（`output.py`）：WAV 以有界块流式写出，校验帧数后 `os.replace` 原子替换；
  verify 复用分块摘要逐字节定位损坏位置。

## 性能与内存

60 分钟 48 kHz 单声道素材在本机 4 核 Apple Silicon 上实测峰值内存约 70 MB、
整条流水线约 3 秒，远低于 1.5 GB 上限。内存有界的原因是：逐块读取与写 PCM、
静音扫描按 30 秒块流式处理、最终 WAV 按 1 MiB 块流式封装。

## 约束

- 仅处理本地输入，不联网。
- 不覆盖源素材：输出写入 `output/`，中间文件在 `work/`。
- 随机样例使用固定种子（12785），重复生成结果一致。
- 时间计算使用整数采样点与有理数帧率。
