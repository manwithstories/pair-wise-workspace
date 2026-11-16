# audio_segmenter

本地 PCM WAV 的对白 / 环境声 / 短音效自动切片工具。识别有效音频片段与静音区间，
输出采样点与秒级边界、峰值与平均 RMS，供批量切片、事件标注与离线资源检查使用。

纯本地运行：Python 3.11+、NumPy、标准库 `sqlite3`/`argparse`/`wave`/`json`/`unittest`。
不依赖音频处理框架、独立数据库或任何外部服务。

## 快速开始

```bash
# 生成夹具 + 检测 + 落库 + 导出 + 校验
python scripts/run_demo.py --db demo.sqlite --out demo.jsonl

# 对已生成的夹具目录做批处理
python -m src.audio_segmenter.cli batch \
  --input demo_data --db batch.sqlite --jsonl batch.jsonl \
  --sample-rate 48000 --threshold-db -42 --min-silence-ms 120

# 校验（可针对输入目录或已有数据库）
python -m src.audio_segmenter.cli verify --input demo_data
python -m src.audio_segmenter.cli verify --db batch.sqlite

# 单元测试
python -m unittest discover -s tests -p 'test_*.py'
```

## 子命令

| 命令 | 作用 |
|---|---|
| `generate` | 生成确定性夹具（含 `invalid/` 负向样例） |
| `detect` | 检测单文件或目录，打印片段明细，可导出 JSONL/CSV |
| `batch` | 并行处理多个文件，写入 SQLite 并批量导出 |
| `verify` | 检查采样率、边界单调、互不重叠、时长守恒 |

退出码：`0` 通过，`2` 参数或输入格式错误，`3` 检测或数据库失败，`4` 校验不通过。

## 检测流程

1. 按采样点分帧（默认 20 ms 帧长 / 10 ms 步长），双声道取算术均值
2. 用低分位估计背景噪声底（排除数字静音帧）
3. 绝对阈值 `--threshold-db` 与相对阈值 `noise_floor + margin` 取高者
4. 滞回：进入用高阈值，退出用 `enter - hysteresis`，阈值内短暂跌落并入片段
5. 渐入渐出补偿：沿单调能量趋势外扩边界，避免把淡入淡出判成静音
6. 桥接短于 `--min-silence-ms` 且短于 `max_pulse_ms` 的间隙，再合并、过滤短片段
7. 按 `--padding-ms` 向相邻静音补齐（相邻片段各取间隙的一半，永不重叠）
8. 静音区 = 片段在 `[0, total_samples)` 上的补集，因此时长恒等成立

## 模块

```
src/audio_segmenter/
  wavio.py      PCM16/PCM24 读取与校验，流式 WavReader
  signal.py     分帧、通道合并、RMS/峰值、能量平滑
  detector.py   噪声底、滞回、渐入渐出补偿、检测主流程
  regions.py    区间代数：掩码成段、合并、补齐、取补
  model.py      片段、静音区、检测配置
  store.py      sqlite3 持久化（内容哈希幂等）
  exporter.py   JSONL / CSV / SQLite 导出
  pipeline.py   批处理编排与单文件失败隔离
  verify.py     不变式校验
  reporting.py  终端输出契约
  fixtures.py   夹具合成
  cli.py        generate / detect / batch / verify
```

内存只随帧网格（`总采样数 / 步长`）增长，音频按块流式读取，不整段驻留。
