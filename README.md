# Offline Podcast Mixer

一套纯本地的离屏多轨混音工具。工程 JSON 描述轨道、自动化、总线和节点连接；同一个执行器既能把整段当一个块渲染，也能按固定 chunk 流式推进。所有命令只在 stdout 输出一行 JSON，便于脚本解析。

## 安装与验收

```bash
python -m pip install -r requirements.txt
python tools/make_fixtures.py --root fixtures/show --rates 44100,48000,96000
python -m mixer.cli validate fixtures/show/mix.json
python -m mixer.cli render fixtures/show/mix.json --out outputs/mix.wav --report outputs/report.json
python -m mixer.cli stream fixtures/show/mix.json --chunk 4096 --report outputs/report_stream.json
python -m mixer.cli parity fixtures/show/mix.json --chunk 4096 --tolerance 1e-6
python -m mixer.cli report outputs/report.json --track host
```

夹具脚本生成 8 轨、44.1/48/96 kHz、单声道/立体声、静音轨、零帧空轨和满幅片段；工程文件写为 `fixtures/show/mix.json`。`fixtures/`、`outputs/`、报告和 `__pycache__` 都不进入版本库。

`stream` 可额外传 `--out path.wav` 边算边写。未传 `--out` 时只计算并给出报告，不保留整段音频。

## 清单结构

```json
{
  "version": 1,
  "sample_rate": 48000,
  "channels": 2,
  "duration_s": 12.0,
  "buses": [{"id": "mix", "gain_db": 0.0, "ceiling": 0.98, "softness": 1.0}],
  "tracks": [
    {
      "id": "host",
      "file": "audio/host.wav",
      "gain_db": -3.0,
      "pan": -0.2,
      "automation": [
        {"type": "linear", "start": 0.0, "end": 4.0, "from_db": -6.0, "to_db": -2.5},
        {"type": "hold", "start": 4.0, "end": 12.0, "from_db": -2.5, "to_db": -2.5}
      ]
    }
  ],
  "nodes": [
    {"id": "host.read", "type": "read", "track": "host"},
    {"id": "host.gain", "type": "gain", "track": "host", "input": "host.read"},
    {"id": "host.pan", "type": "pan", "track": "host", "input": "host.gain"},
    {"id": "mix.sum", "type": "sum", "inputs": {"in0": "host.pan"}},
    {"id": "mix.trim", "type": "trim", "max_samples": 576000, "input": "mix.sum"},
    {"id": "mix.limiter", "type": "limiter", "ceiling": 0.98, "softness": 1.0, "input": "mix.trim"}
  ],
  "output_node": "mix.limiter"
}
```

路径相对清单文件解析。`duration_s` 省略时取所有轨道换算到项目率后的最大长度。`nodes` 省略时会自动生成 `read -> gain -> pan -> sum -> gain -> trim -> limiter` 的默认图。

节点类型与输入端口：

- `read` / `track` / `source`：0 个输入，必须写 `track`。
- `gain`：`in`。有 `track` 时应用轨道静态增益和自动化；额外 `gain_db` 会再叠加。
- `pan`：`in`。有 `track` 时使用轨道声像，否则用节点参数 `pan`。
- `sum`：至少一个输入，端口必须连续命名为 `in0`、`in1` …；缺口会被判为悬空端口。
- `trim`：`in`，可选 `max_samples`，按工程内绝对帧位置截断。
- `limiter` / `softclip`：`in`，`ceiling` 在 `(0, 1]`，`softness >= 1`。

图会做拓扑排序和环检测。未知节点类型、重复轨道或节点 id、缺失素材、非法 dB、采样率为 0、悬空端口和环都以退出码 2 拒绝，并在 `details` 中给出节点或轨道 id。

## 自动化与电平语义

- 曲线段支持 `linear`、`exponential`、`hold`。也可用 `points: [{"t": ..., "gain_db": ...}]` 写多条折线点。
- 自动化固定按 1 kHz 控制率采样，再在相邻控制点间线性插值到每个音频样本，最后把 dB 转成线性增益。
- `exponential` 在端点线性增益之间做指数插值；`hold` 保持段起点值。
- 时间早于第一段取首端点，晚于最后一段取末端点；区间之间取前一段末端值。
- 重采样使用 32 抽头窗函数 sinc 多相滤波器，输入历史与前瞻跨块保留；短轨补零、长轨截断并写入 `warnings`。
- 轨道统计取自该轨最后一个带 `track` 标记的节点，也可用轨道字段 `meter_node` 显式指定。
- 峰值与 RMS 均为 dBFS。无样本或全静音输出 JSON 字符串 `"-inf"`，保持输出是标准 JSON。
- 削波定义是 `abs(sample) >= 1.0` 的标量样本数。总线同时报告限幅前和限幅后的计数，`clip_count` 等于限幅后的计数。

## 输出与退出码

`render`、`stream` 的 JSON 包含 `tracks`、`automation`、`automation_points`、`mix` 和 `warnings`。`report --track` 输出该轨的峰值、RMS、样本数、重采样长度、增益范围、拐点数量和总线摘要。

`parity` 先整段渲染，再按 chunk 流式渲染并逐样本比较，输出 `max_abs_diff`、`chunks`、`ok`。写入磁盘的 WAV 是 32-bit float WAV；libsndfile 的 `PEAK` 墙钟时间戳会被归零，所以相同输入重复渲染以及整段/流式路径写出的 WAV 都逐字节一致。

退出码：

- `0`：成功。
- `2`：清单或混音图校验失败。
- `3`：渲染、流式处理或 parity 超差失败。
- `4`：文件读写失败。

渲染和流式写盘都使用同目录临时文件，校验与处理完成后才原子替换目标文件；任一异常都会删除临时 WAV/报告，不留下半成品。

## 测试

```bash
pytest -q
```

测试覆盖三类自动化曲线及边界、四组采样率跨块重采样、双路径逐样本一致、WAV 字节确定性、图环/悬空端口、清单错误、损坏 WAV、NaN、三声道素材、不可写输出路径和 parity 超差退出码。

## 性能参考

在本地 8 轨、48 kHz、300 秒、立体声线性总线的工程上实测：整段 `render` 约 1.2 秒、峰值内存约 1.1 GB；同工程 `stream --chunk 4096` 约 1.7 秒、常驻峰值约 33 MB。混合采样率工程会因 sinc 重采样增加耗时，但两条路径的块调度与节点代码相同。
