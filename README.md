# Aircheck

电视台母版车间的多轨素材检查与无损再封装工具。纯本地运行，无网络依赖，面向
macOS（ARM64 / x86_64）终端使用。

## 环境

```sh
python3.12 -m venv .venv
.venv/bin/python -m pip install -e '.[test]'
```

依赖固定在 PyAV 12.x、NumPy 2.x、soundfile 0.12.x。所有媒体素材由程序在运行时
生成，仓库不包含任何二进制夹具。

## 验收

```sh
.venv/bin/python -m aircheck.cli demo \
  --formats mov,mp4,mkv --queue-size 4 --slow-consumer-ms 25 --output /tmp/aircheck-out

.venv/bin/python -m aircheck.cli verify --input /tmp/aircheck-out --json
```

`demo` 打印（`--json` 时输出结构化文档）：

```
roundtrip_parse_rate=1.000
keyframe_hit_rate=1.000
segment_hash_match=1.000
peak_queue_items=4
peak_rss_mb=...
damaged_segments=9
```

退出码：`0` 通过，`1` 验收差异，`2` 参数或输入错误，`3` 不可恢复处理失败。

## 子命令

| 命令 | 作用 |
| --- | --- |
| `demo` | 生成正常/损坏素材，跑完整流水线并汇总验收指标 |
| `verify` | 对已有 `demo` 输出目录独立复核（解析率、逐片段哈希、关键帧命中） |
| `probe` | 打印容器与流元数据，含尾部截断/损坏包/无索引/时间戳问题 |
| `map` | 校验 `v:0=a:1` 形式映射与时间线（负值、非单调、剪辑点、转场重叠） |
| `remux` | 单个素材无损再封装；`--resume` 续跑，`--cancel-after-ms` 演示中断 |
| `index` | 生成关键帧索引并校验命中率、重复、错序、越界偏移 |
| `audio` | 抽样声道数、峰值与静音区间，报告异常采样率与解码失败 |

所有子命令都支持 `--json`，便于值班脚本消费。

## 架构

```
src/aircheck/
  timebase.py   整数纳秒刻度、时间戳换算、统一错误模型与退出码
  probe.py      容器/流元数据、损坏分类、MKV Cues 结构校验
  demux.py      按片段归类的逐包解复用，注入边界标记，可取消
  mapping.py    轨道映射与时间线/转场校验
  queueing.py   有界队列（条目+字节双上限），put/get 可被取消唤醒
  remux.py      无损再封装：原样转发 pts/dts/duration/载荷
  hashing.py    逐片段、逐流 SHA-256 载荷与身份摘要，副本按包序号对齐
  keyframes.py  关键帧索引生成与命中率/重复/错序/越界校验
  audio.py      抽样音频检测（声道、峰值、静音区间）
  pcm.py        用 soundfile 独立解码 PCM，逐样本核对
  mkvwrite.py   最小 EBML 写入器，用于构造坏索引/回绕/错序夹具
  fixtures.py   确定性生成正常与损坏素材
  pipeline.py   三阶段背压流水线、取消、检查点与续跑
  demo.py       demo/verify 编排与验收汇总
  cli.py        参数解析与终端输出
```

### 设计要点

- **整数时间基准**：所有时间戳换算走 `Fraction` + 整数纳秒，避免长素材上的
  浮点漂移；校验时钟与转发时钟分离，无损拷贝始终回写原始 `pts/dts`。
- **有界背压**：解复用、映射、写入三个线程由两条有界队列串联。队列满即阻塞
  上游，峰值深度写入验收报告；取消标志能唤醒阻塞在队列上的线程。
- **精确续跑**：片段边界以标记形式随数据流经队列，写入端只在标记到达后提交整段。
  容器无法原地追加，续跑写入独立 `partN` 文件；取消时丢弃未提交持有区，因而
  任一片段都不会被写两次。
- **逐片段哈希**：按“片段内逐流”比对，容器间的包交错顺序差异不算损坏；跨容器
  时间戳会因时基与预热不同而轻微漂移，单独按容差报告。
- **损坏隔离**：尾部截断、损坏包、无索引、时间戳回绕/错序分别归类，单个素材
  失败不吞掉上下文，也不影响后续素材。

## 测试

```sh
.venv/bin/python -m pytest
```

覆盖时间基准与错误模型、映射与时间线、损坏分类、九组容器互转的逐片段哈希、
背压上限、取消/续跑无重复、关键帧与音频检查、CLI 退出码。
