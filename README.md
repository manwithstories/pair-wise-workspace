# mixctl

本地多轨混音流水线：统一采样率、自动化曲线、离线/流式双路径渲染与削波指标。
纯终端、纯本地、无外部服务依赖。

## 安装

```bash
pip install -r requirements.txt
```

依赖仅 `numpy==1.26.4` 与 `soundfile==0.12.1`，需要 Python 3.11+。

## 演示命令

```bash
python -m mixctl fixtures --out fixtures
python -m mixctl render --project fixtures/demo.mix.json \
    --out out/mix.wav --report out/report.json
python -m mixctl render --project fixtures/demo.mix.json \
    --engine stream --chunk 777 --out out/stream.wav
python -m mixctl compare --a out/mix.wav --b out/stream.wav --json
```

`compare` 成功时输出形如：

```json
{"identical": true, "max_abs_diff": 0.0, "samples": 2880000}
```

## 模块划分

| 模块 | 职责 |
| --- | --- |
| `cli.py` | 子命令、参数解析、退出码映射 |
| `project.py` | 工程 JSON 解析与字段路径校验 |
| `automation.py` | 关键帧求值、hop 控制速率、逐样本增益 |
| `resample.py` | 有理比多相重采样（流式、块无关） |
| `graph.py` | 混音 DAG、拓扑排序、静音/独奏、成环检测 |
| `engine_common.py` | 两引擎共享的轨道运行时与总线求和 |
| `engine_offline.py` | 整轨离线渲染 |
| `engine_stream.py` | 固定块长流式渲染，内存不随时长增长 |
| `metrics.py` | 峰值、RMS、削波、自动化控制点统计 |
| `pipeline.py` | 顶层调度、24-bit WAV 原子写出、报告组装 |
| `fixtures.py` | 生成 5 轨演示工程与源 WAV |

## 关键设计

- **有理比重采样**：`target/source` 经 GCD 化简（44.1k→48k 为 `160/147`），
  多相 FIR 组按整数输入游标推进，状态只随输入前进，任意块长逐字节复现。
- **块无关自动化**：关键帧时间预先换算成采样下标，插值在整数下标空间用
  显式区间公式完成，不使用对控制点子集敏感的 `np.interp`，增益只依赖全局
  采样下标，块边界不重置相位。
- **双路径一致**：离线与流式共用同一 `RenderSession`、`Automation` 与
  `Resampler` 类，差异只在数据投喂粒度。
- **数值与输出**：全程 float64，写出前钳制到 `[-1, 1]`，输出 24-bit PCM WAV；
  dBFS 以满幅 1.0 为 0；静音轨 RMS/峰值输出 JSON `null`。
- **原子写出**：WAV 与 JSON 先写临时文件再 `os.replace`，失败时不留半成品。

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 成功 |
| 2 | 参数或工程错误（`ProjectError`） |
| 3 | 音频读写错误（缺文件、损坏、声道不符） |
| 4 | 混音图无效（未知总线、成环） |
| 5 | 双路径不一致（`compare` 差异） |

## 测试

```bash
python -m unittest discover -s tests
```
