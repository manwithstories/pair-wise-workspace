# relaydiag

区域直播分发链路的节目流损坏排查与检查点恢复工具。纯本地、纯终端，无外部
服务依赖。它在统一有理数时间基准上完成包/帧/音频块探测、节点时钟漂移拟合
与 32 位时间戳回绕展开、关键帧节奏与缺帧/坏包分析，并把每一处异常映射到
**可复核的源字节偏移与时间区间**，最后输出带逐段校验和的确定性 JSON 清单。

## 技术栈

- Python 3.12，依赖仅安装于项目根目录 `.venv`
- PyAV 12.3（H.264/AAC 真实编解码）、NumPy 2.1、soundfile 0.12（WAV 第二轨）
- 系统依赖 FFmpeg 7.x / libsndfile（Homebrew）

## 安装

```bash
brew install ffmpeg pkg-config libsndfile
python3.12 -m venv .venv
.venv/bin/python -m pip install 'av==12.3.0' 'numpy==2.1.0' 'soundfile==0.12.1'
.venv/bin/python -m pip install -e .
```

## 演示流程

```bash
# 1. 生成确定性素材（视频/音频/多轨剪辑/转场 + 注入的损坏与回绕）
.venv/bin/python -m relaydiag generate --out /tmp/relay-fixture

# 2. 诊断并写检查点与结果清单（发现损坏时退出码 3）
.venv/bin/python -m relaydiag run \
  --input /tmp/relay-fixture \
  --checkpoint /tmp/relay-state.json \
  --json /tmp/relay-result.json

# 3. 进程中断后续跑（--resume 校验输入指纹/参数/偏移后恢复）
.venv/bin/python -m relaydiag run \
  --input /tmp/relay-fixture \
  --checkpoint /tmp/relay-state.json \
  --resume \
  --json /tmp/relay-result.json

# 4. 校验清单与重新分析结果一致
.venv/bin/python -m relaydiag verify \
  --input /tmp/relay-fixture \
  --result /tmp/relay-result.json
```

终端摘要包含验收指标键：

| 指标 | 阈值 |
| --- | --- |
| `damage_offset_error_bytes` | ≤ 188 字节 |
| `damage_time_error_ms` | ≤ 40 毫秒 |
| `resume_seconds` | ≤ 2 秒 |
| `output_sha256` | 64 位十六进制 |
| `verified` | `true` |

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 成功 / 校验通过 / 可恢复中断 |
| 2 | 输入或参数错误 |
| 3 | 发现损坏 |
| 4 | 恢复状态不匹配（指纹/参数/偏移） |
| 5 | 输出校验失败 |

## 模块结构

```
src/relaydiag/
  timeline.py    统一有理数时间基准、时间区间、回绕增量
  clock.py       模块化时间戳展开 + 锚点漂移拟合/突变检测/置信度
  container.py   RLYD 确定性容器，记录每包精确源字节偏移
  generator.py   确定性素材生成与异常/损坏计划注入
  probe.py       有界内存流式探测包/帧/音频块与真实编码参数
  keyframes.py   实测关键帧间隔，区分 VFR/缺帧/坏包
  damage.py      异常 -> 字节偏移 + 时间区间，转场跨轨连续性
  pipeline.py    固定容量队列、背压、失败检查点（取消安全关停）
  checkpoint.py  原子保存/恢复，输入指纹与参数校验
  manifest.py    确定性 JSON 摘要与逐段 SHA-256 校验和
  engine.py      端到端编排与验收指标
  cli.py         generate / run / verify 子命令
```

## 设计要点

- **有界内存**：探测与管线只保留每轨标量状态和固定容量队列；对 50 万包 /
  125 MiB 输入实测算法层内存增长为 0，常驻远低于 64 MiB。
- **背压与恢复**：队列满即阻塞上游；队列饱和、输入截断、磁盘写入失败都会
  原子落检查点。取消事件保证任何阶段失败后所有线程干净退出，无死锁。
- **恢复一致性**：检查点保存输入 SHA-256、大小、规范化参数与下一偏移；任一
  不匹配拒绝续跑（退出码 4）。续跑产出的损坏记录与一次性运行逐字段一致。
- **确定性**：所有随机数带种子，JSON 以排序键 + 固定分隔符规范化，重复运行
  的 `output_sha256` 完全相同。

## 测试

```bash
.venv/bin/python -m pytest tests/ -v
```

覆盖回绕、漂移突变、转场边界、截断、进程中断恢复、状态/参数不匹配拒绝以及
清单确定性与全部退出码。
