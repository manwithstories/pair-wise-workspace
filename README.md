# MixGraph

纯本地终端混音工具：用可版本管理的 JSON 描述轨道、总线、发送和逐样本增益自动化，并让离线烘焙与运行时流式处理共用同一套图、重采样、自动化和指标实现。

## 快速验收

```bash
python -m mixgraph gen-fixtures --out assets --seed 7 --project demo/project.json
python -m mixgraph render --project demo/project.json --out outputs/mix.wav --report outputs/render.json
python -m mixgraph stream --project demo/project.json --block 256 --out outputs/stream.wav --report outputs/stream.json
python -m mixgraph curves --project demo/project.json --node vo --times 0,1.25,3.5
python -m mixgraph verify --project demo/project.json --tol 1e-6
```

`gen-fixtures` 只用伪随机数和数学波形生成素材，不读取仓库外文件。示例包含三档采样率（44.1/48/96 kHz）、单声道/立体声、四轨和两条总线。

## 工程格式

```json
{
  "sample_rate": 48000,
  "channels": 2,
  "tracks": [
    {
      "name": "vo",
      "source": "../assets/vo_44k1_mono.wav",
      "gain_db": 0.5,
      "mute": false,
      "solo": false,
      "sends": [{"bus": "dialogue_bus", "gain_db": 0.0}],
      "automation": {
        "points": [
          {"time": 0.0, "gain_db": -3.0, "curve": "linear"},
          {"time": 1.25, "gain_db": -4.0, "curve": "exponential"}
        ]
      }
    }
  ],
  "buses": [
    {"name": "dialogue_bus", "gain_db": 0.0, "sends": [{"bus": "master", "gain_db": 0.0}]}
  ],
  "master": {"name": "master", "gain_db": -1.5}
}
```

- `bus` 是单路发送的简写；完整写法使用 `sends`，可多路发送并各自带增益。轨道和总线都可以发送到总线，总线最终必须能到达 `master`。
- 节点名（轨道、总线、主输出）全局唯一；发送目标不存在、重名、环路和无法到达主输出的悬空总线都会在载入时拒绝，并在错误信息中给出节点标识。
- `linear` 段在**线性幅度**上插值，`exponential` 段在 **dB** 上插值，`hold` 段保持起点值。段由当前断点的 `curve` 描述；第一个断点之前和最后一个断点之后保持端点。
- 断点时间必须有限、非负且严格递增；空断点列表会被拒绝。

## 渲染与指标

`render` 和 `stream` 使用同一个 `MixEngine`。静态增益、静音、独奏和自动化在轨道信号上相乘；独奏门只影响实际混音，不影响逐轨指标上报。轨道指标统计其真实素材长度，不把项目尾部的补零算进 RMS。

- `peak_dbfs`：所有声道样本绝对峰值的 dBFS。
- `rms_dbfs`：所有声道样本的均方根 dBFS。
- `clip_samples`：`abs(sample) > 1.0` 的声道样本数。
- 静音或空信号统一报告 `-120.0 dBFS`，保证 JSON 始终可解析。

`stream --block` 接受任意正的 2 的幂。内部按定长块读取和求和；重采样器保存跨块滤波器状态，结束时冲刷尾部。三种常用块长 `64/256/1024` 的报告和音频样本一致。加 `--print-metrics` 后每块输出一行 JSON 增量指标。

`verify` 以 4096 帧离线块和 256 帧流式块同时推进，逐样本比较，输出：

```json
{"max_abs_diff": 0.0, "pass": true}
```

## 退出码

| 退出码 | 含义 |
| --- | --- |
| 0 | 成功 |
| 2 | 命令行参数错误，含非 2 的幂块长 |
| 3 | 素材缺失/损坏、声道数非法、采样率非法或读写失败 |
| 4 | 工程图非法，含空/乱序自动化、环路、悬空总线、重名节点 |
| 5 | 一致性与容差校验失败 |

错误会写到 stderr，并带有 `error[kind]` 前缀，便于终端脚本区分具体链路。

## 模块

`mixgraph/` 下九个实现模块：

| 模块 | 职责 |
| --- | --- |
| `cli.py` | 子命令、参数校验、退出码 |
| `graph.py` | 图模型、路由校验、拓扑排序 |
| `automation.py` | 三类增益段与逐样本求值 |
| `resample.py` | 有理数比多相抗混叠重采样 |
| `assets.py` | 夹具生成、WAV 读写、分块读取 |
| `offline.py` | 整段离线渲染与一致性比较 |
| `streaming.py` | 定长块引擎、流式写入、尾部冲刷 |
| `meters.py` | 峰值、RMS、削波统计 |
| `report.py` | JSON 报告与终端摘要 |

`mixgraph/__main__.py` 只是让 `python -m mixgraph` 可执行的 3 行入口，不承载业务逻辑。

## 验证

```bash
python -m pytest -q
```

测试覆盖自动化插值与拒绝、环路/悬空/重名、重采样块不变性与抗混叠、削波计数、三种块长一致性、独奏上报不变性以及各错误退出码。流式内存只与块长、节点数和滤波器宽度相关，不随时长增长。
