# talkmap

流式 WAV 说话段 / 静音段分割工具，面向电竞赛事赛后点播剪辑。纯 Python + NumPy 实现，
无图形界面、无网络、无外部进程依赖，全部能力通过终端子命令使用。

## 安装

```bash
python -m pip install -e .
```

安装后提供 `talkmap` 入口，也可用 `python -m talkmap` 调用。

## 数据管线

`__main__` → `wavio`（流式 RIFF 分块读取）→ `framing`（20ms 帧 / 10ms 跳距 RMS、dBFS）
→ `noise`（2 秒块 10% 分位 + 跨块中值平滑 + 跃变分段）→ `detect`（底噪 +8dB 进入、
+12dB 退出的双阈值滞回）→ `refine`（150ms 短静音 / 200ms 短说话合并、渐变边界、
120ms 尾延、余量钳制）→ `store`（sqlite 幂等落库）。`export` 复用同一检测管线。
依赖方向单向，命令层只向下调用。

## 命令

```bash
# 生成确定性夹具（4 个不同位深/声道/噪声跃变的 WAV + truth.json）
python -m talkmap synth --out fixtures --seed 7 --masters 4 --minutes 3

# 分析并落库，输出 files / segments / silence_seconds / segments_by_file
python -m talkmap analyze "fixtures/*.wav" --db index.sqlite3 --json

# 复用同一管线按段导出试听 WAV 与 manifest.jsonl（原子写、重叠按中点切分）
python -m talkmap export --db index.sqlite3 --out exports --pre-roll-ms 40 --post-roll-ms 120

# 按文件 / 最短静音 / 时间窗查询
python -m talkmap query --db index.sqlite3 --min-silence-ms 400 --json

# 与夹具真值比对边界误差
python -m talkmap check --db index.sqlite3 --fixtures fixtures --json
```

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 成功 |
| 1 | 查询无匹配 |
| 2 | 参数错误 |
| 3 | 音频不可读（非 PCM、容器损坏等） |
| 4 | 数据库失败 |

## 约束

- 仅处理工作区内的本地 WAV，路径越界直接拒绝。
- 16 / 24 / 32 位 PCM，单 / 双声道自动下混；未知 RIFF chunk 跳过；data 声明超长按实际截断并告警。
- 边界映射回真实采样点，尾部不补零外推；导出首尾钳制在 `[0, 总采样数]`。
- 检测结果以 `sha256 + 参数快照` 为键幂等 upsert，参数变更保留旧行并置 `superseded`。
- 流式处理，90 分钟单声道峰值内存远低于 200 MB。

## 测试

```bash
python -m pytest tests/ -q
```
