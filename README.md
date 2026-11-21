# avtimeline — 多人竞技回放的多轨时间线分片

纯本地、确定性的媒体处理示例：把选手视角画面、队伍语音与战术标记整理成
一条共享时间线，统一采样率、声道布局与色彩元数据，按同一组分片边界输出多
码率表示层与共用清单，并支持从损坏素材或中断阶段续跑。无网络、无设备权限、
无图形界面，仅运行于 macOS 本机与 Python 3.12。

## 依赖

| 包 | 版本 | 用途 |
|----|------|------|
| PyAV | 12.x | 解复用、解码、重采样与 H.264/AAC/FFV1 编解码（自带 FFmpeg wheel） |
| NumPy | 2.x | 时间线合成、音频混音与像素处理 |
| soundfile | 0.12.x | 读取 WAV 源与校验样本数 |

`pyproject.toml` 使用项目自带的 `build_backend.py`（PEP 517）完成安装，
不依赖 setuptools，也不在构建期联网。`[test]` 额外依赖为空，测试使用标准库
`unittest`。

## 安装与验收

```sh
python3.12 -m venv .venv
.venv/bin/python -m pip install -e '.[test]'
.venv/bin/python -m avtimeline demo \
  --workdir /tmp/avtimeline-demo \
  --break-at encode --resume --corrupt-sample --edge-sample
.venv/bin/python -m unittest discover -s tests -v
```

第一条命令会：生成全部样例素材 → 跑到 `encode` 后模拟中断 → 从检查点续跑 →
在独立临时目录重复完整处理并比对校验和 → 输出 JSON 摘要。第二条命令运行
全部用例并打印通过与失败数。

### 摘要字段

| 字段 | 含义 |
|------|------|
| `first_error_track` / `first_error_pts` | 首个异常所在轨道与媒体时间（误差不超过 1 帧或 1 个音频块） |
| `first_error_frame` / `first_error_sample` | 视频帧号或音频采样号（若为对应流） |
| `resume_seconds` / `resume_ratio` | 续跑耗时与相对完整重跑的占比（目标 < 50%） |
| `aligned_segments` | 各表示层边界完全一致的分片数（≥ 6） |
| `boundary_error` | 帧/采样号与有理时间的最大偏差（应为 0） |
| `sha256_match` | 独立重复运行的分片与清单校验和是否全部一致 |
| `short_segments` | 不足一帧或一个采样块的尾段及其实际时长 |
| `color_decision` | 色彩策略、输出属性、各来源与冲突 |
| `skipped_tracks` | skip 策略下被剔除的损坏轨道 |

### 退出码

| 码 | 含义 |
|----|------|
| 0 | 处理与校验成功 |
| 2 | 素材或时间线无效（无法打开、时间线结构错误、stop 策略遇到坏素材） |
| 3 | 恢复或输出校验失败（对齐失败、校验和不一致、恢复超时） |

其余子命令：

```sh
# 两次独立完整运行并逐片比对
.venv/bin/python -m avtimeline verify --workdir /tmp/avtimeline-verify

# 解析并校验时间线文档
.venv/bin/python -m avtimeline inspect path/to/timeline.json
```

## 模块

```
src/avtimeline/model.py       有理数时间基准、区间、速率与时间格式化
src/avtimeline/errors.py      结构化错误模型、阶段枚举与退出码
src/avtimeline/timeline.py    时间线解析与轨道/覆盖/剪辑/转场校验
src/avtimeline/probe.py       流元数据探测、关键帧、截断与解码错误定位
src/avtimeline/normalize.py   采样率/声道归一、色彩策略与无损中间件
src/avtimeline/compose.py     节目合成：视频转场、音频铺轨与混音
src/avtimeline/segment.py     多表示层共用的分片边界与短尾段
src/avtimeline/encode.py      每表示层分片编码（首帧关键帧、字节确定）
src/avtimeline/manifest.py    共用清单、跨层对齐校验与 SHA-256
src/avtimeline/graph.py       阶段依赖图与环检测
src/avtimeline/checkpoint.py  原子检查点、输入/产物摘要与恢复位置
src/avtimeline/pipeline.py    阶段调度、策略、断点续跑与摘要
src/avtimeline/samples.py     固定种子样例生成（含损坏与边界素材）
src/avtimeline/wavio.py       规范 WAV 写出（避免时间戳型元数据）
src/avtimeline/records.py     探测/归一/分片计划的 JSON 记录
src/avtimeline/config.py      配置与工作目录布局
src/avtimeline/cli.py         demo / verify / inspect 入口
```

## 关键设计

- **精确时间**：所有时间都是 `Fraction`；浮点只用于显示。分片边界同时以
  有理时间、帧号与采样号表达，对齐校验要求三者零误差。
- **统一归一**：音频经 swresample 统一到 48 kHz 立体声并按容器声明裁剪编码器
  填充；视频转为 FFV1 无损中间件并写入选定色彩属性；色彩冲突时按
  `first`/`track`/`explicit` 策略决定并记录来源。
- **损坏定位**：解码前缀与容器声明长度比对，截断点定位到具体帧号或采样
  区间；错误按媒体时间、轨道、阶段排序，摘要给出首个异常。
- **续跑语义**：检查点在阶段完成后原子落盘，记录输入摘要与每个产物摘要；
  加载时校验版本、输入与全部产物，失效则从最早失效阶段重做，其余产物复用。
- **确定性**：编码参数固定、单线程；WAV 自写头部；清单按键排序。相同输入与
  配置在独立目录重复处理得到相同时间线、清单与逐片校验和。
- **资源约束**：有界解码与合成帧数守卫，内存控制在 1 GiB 以内；错误与摘要
  不包含任何媒体载荷。
