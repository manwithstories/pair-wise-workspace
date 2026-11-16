# 离线 / 流式混音母带（48 kHz 立体声）

同一套混音图、增益求值器与限幅器同时驱动**离线整段渲染**与**流式分块渲染**，
两条路径输出逐样本一致（当前夹具最大绝对误差约 `3e-8`，远小于 `1e-5` 阈值）。

```
夹具生成器 → IO(校验/读取) → 重采样(→48k) → 混音图(track/bus/master)
            → 自动化(分段线性+斜率限制) → 限幅(峰值软限幅) → 母带 WAV
                                    ↑
                        离线引擎 / 流式引擎共享同一 graph 与 evaluator
```

## 环境

```bash
python -m pip install numpy==1.26.4 soundfile==0.12.1 pytest==8.3.3
```

无外部服务、无网络、无密钥；所有素材由种子生成，RNG 状态贯穿整条链路。

## 验收命令

```bash
python -m fixtures.generate_samples --out /tmp/mix-fixtures --seed 17
python -m src.mixer.cli render --input /tmp/mix-fixtures/project.json --output /tmp/offline.wav --rate 48000 --block 1024
python -m src.mixer.cli stream --input /tmp/mix-fixtures/project.json --output /tmp/stream.wav --rate 48000 --block 256
python -m src.mixer.cli compare --offline /tmp/offline.wav --stream /tmp/stream.wav --automation /tmp/mix-fixtures/project.json
```

期望值（种子 17）：

| 指标 | 期望 | 实测 |
|---|---|---|
| `tracks` | 4 | 4 |
| `target_rate` | 48000 | 48000 |
| `frames` | 数十万 | 192000 |
| `automation_points` | 8–32 | 17 |
| `peak_dbfs` | −1…0 | −0.5 |
| `rms_dbfs` | −24…−6 | −23.16 |
| `clip_count` | 0…数十 | 0 |
| `max_error` | ≤1e-5 | ≈3e-8 |

## 子命令

- `generate --out DIR [--seed N]`：生成 4 条素材（44.1 kHz 单声道、48 kHz 立体声、
  96 kHz 四声道、48 kHz 双片段立体声）与 `project.json`。
- `render --input P --output W --block N`：一次处理完整时间线。
- `stream --input P --output W --block {256,1024} [--simulate short|empty|ragged]`：
  按固定块流式渲染；`--simulate` 注入短读、空块或锯齿块，用于演示分块鲁棒性。
- `inspect --path P`：项目摘要（轨道/布局/自动化）或 WAV 摘要。
- `compare --offline A --stream B [--automation P] [--max-error X]`：
  比较帧数、最大绝对误差、MSE、自动化采样点（`sampled_points`：离线边缘数、
  256/1024 分块数）与逐轨统计；`rendered_limiter` 附带限幅前后峰值/有效值、
  削波计数与最大瞬时增益变化。

退出码：`0` 全部通过；`2` 参数或输入错误；`3` 素材格式错误；`4` 离线与流式超差。

## 项目 JSON

```json
{
  "schema": "mix-project/1",
  "target_rate": 48000,
  "buses": [{"name": "rhythm", "tracks": ["drums", "bass"], "gain_db": -1.0}],
  "tracks": [{
    "name": "drums",
    "file": "track1_mono_44100.wav",
    "layout": "mono",                 // mono | stereo | quad（未知布局→退出码 3）
    "start_time": 0.0,                // 母带时间线上的延迟（秒）
    "gain_db": 3.0,
    "silences": [[1.1, 1.5]],         // 母带时间线秒；区间内置零
    "clips": [{"path": "...", "start_time": 0.0,
               "source_offset_frames": 0, "length_frames": 24000}],
    "channel_matrix": [[1,0],[0,1],...],   // 可选，覆盖布局默认折叠矩阵
    "automation": [{"time": 0.0, "gain_db": 3.0}, {"time": 0.9, "gain_db": 6.0}],
    "max_slew_db_per_s": 40.0
  }]
}
```

- 自动化节点按时间排序；同一时间点重复定义时**最后一条生效**。
- 时间与静音窗均以母带时间线秒为单位；`clips` 的偏移/长度以源文件采样率为单位。
- `automation.time` 与 `silences` 语义一致，便于直接按听感对齐。

## 实现要点

- `src/mixer/model.py`：布局、片段、轨道、自动化点、总线与逐轨统计。
- `src/mixer/graph.py`：track/bus/master 节点，Kahn 拓扑排序 + 分层摘要；
  环路、孤立节点、未连接输出、伪造引用均在渲染前失败。
- `src/mixer/resample.py`：分段线性重采样；按目标窗口反算源窗口，内存只与块大小相关；
  空片段与尾部自动补零。
- `src/mixer/automation.py`：逐帧求值（因此 256/1024 块结果完全相同）、边界夹持、
  斜率限制（重写拐点斜率，确定性且无积分器状态）。
- `src/mixer/limiter.py`：attack/release 用 min-plus 前缀扫描，任意分块结果一致；
  软膝 + 硬回退，逐轨与母带均统计峰值/有效值/削波数/最大增益变化。
- `src/mixer/engine.py`：`MixSession` 被离线与流式共享；流式支持 0 长度块、
  短读与末尾不足一块。
- `src/mixer/io.py`：RIFF 头扫描识别截断，采样率/声道数/非有限值校验并点名轨道。
- `src/mixer/cli.py`：五个子命令，JSON 报告 + 一行人类摘要到 stderr。

## 测试

```bash
python -m pytest tests -q     # 58 passed
```

覆盖：离线/流式逐样本一致（含恶意分块计划）、自动化采样点与边界连续性、
斜率上限、限幅分块无关性、重采样分块一致性、环路/孤立/未连接检测、
非法采样率/截断/NaN/声道不匹配点名轨道、CLI 退出码 0/2/3/4。
