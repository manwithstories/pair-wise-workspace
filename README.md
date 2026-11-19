# archive-restore

历史 HLS/DASH 影视资产的可检索恢复与校验工具。纯本地、终端运行，无外部服务、
无 GUI、不申请摄像头或麦克风权限。所有媒体时间统一使用整数微秒。

## 环境与安装

```bash
brew install ffmpeg
python3.12 -m venv .venv
.venv/bin/python -m pip install -e '.[test]'
```

依赖固定为 PyAV 13.1.0、NumPy 2.1.3、soundfile 0.12.1，全部安装在项目 `.venv` 内。

## 生成示例素材

```bash
.venv/bin/python -m archive_restore.fixtures --out .tmp/archive
```

生成七类场景，每个含两个视频码率 + 一条音频轨：

| 场景 | 特征 |
| --- | --- |
| `normal` | 完整、时间轴对齐 |
| `missing` | 缺段，尝试从相邻同类型码率补齐 |
| `duplicate` | 等时重复片，去重 |
| `outoforder` | 乱序片，按起点重排 |
| `truncated` | 截断清单 + 撕裂分片，保留显式缺口 |
| `rollback` | 时间戳回退 |
| `wraparound` | 计数器回绕，偏移补偿 |

## 恢复与校验

```bash
.venv/bin/python -m archive_restore.cli restore \
  --input .tmp/archive --output .tmp/result \
  --queue-bytes 1048576 --consumer-delay-ms 25 --json

.venv/bin/python -m archive_restore.cli verify \
  --input .tmp/archive --result .tmp/result --json
```

### 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 验证通过 |
| 2 | 输入或参数无效（含越界/绝对路径引用） |
| 3 | 恢复后指标未达阈值 |
| 4 | 不可恢复媒体错误（致命） |

## 关键指标

- `av_sync_error_ms`：音画首端启动差与有效覆盖时长差（毫秒），正常样例 ≤ 40
- `packet_recovery_rate`：恢复分片比例，正常样例 ≥ 0.95；损坏样例给出实测值
- `p95_latency_ms`：端到端阶段延迟 P95，延迟注入后落在 25–500
- `peak_memory_bytes`：有界队列峰值字节，不超过 `--queue-bytes`
- `aligned_representations`：成功对齐的表示层数量（≥ 2）
- `gaps`：无法恢复的显式缺口列表

## 架构

```
清单/生成器 → manifest(HLS/DASH 解析, 路径安全)
           → timeline(统一微秒时间轴, 回退/重复/舍入冲突)
           → segment(目标时长窗口, 关键帧/音频包边界对齐)
           → stream(有界队列, 字节预算背压, 取消/失败传播)
           → recovery(缺片补齐, 去重, 重排, 检查点回滚)
           → metrics(音画同步, 恢复率, 延迟分位, 缺轨可解释指标)
```

输出采用临时文件 + 原子替换；所有媒体路径被限制在输入根目录内，拒绝绝对路径、
目录穿越与清单外引用。

## 测试

```bash
.venv/bin/python -m pytest
```
