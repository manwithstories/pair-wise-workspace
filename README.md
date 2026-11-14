# audioscan

面向职业资格考前串讲录音的纯终端 WAV 切分与静音检测批处理器。项目只依赖 Python 3.11、NumPy、sqlite3、argparse 与 pytest；WAV 容器由本项目自行解析，不调用外部解码器或服务。

## 快速验收

```bash
python -m audioscan synth --out data/fixtures --files 12 --seed 7
python -m audioscan analyze data/fixtures/fx_03.wav --json
python -m audioscan batch enqueue data/fixtures
python -m audioscan batch run --workers 4
python -m audioscan export --out data/cuts --format csv
python -m audioscan report --summary --json
python -m audioscan selfcheck --seed 7
```

`selfcheck` 成功时输出 `matched=12/12`。验收批跑的汇总通常为 `files=12`、`ok=12`、`failed=0`、`segments=60`，静音总毫秒数在五位数范围。

## 支持的容器

- RIFF/WAVE，PCM 16/24/32 位与 IEEE float32。
- 1 到 2 声道，22.05 kHz 到 96 kHz。
- 普通 `fmt ` 与 `WAVE_FORMAT_EXTENSIBLE`。
- 未知块跳过、奇数长度块按 RIFF 规则补齐。
- `data` 越界、头损坏、位深/声道/采样率不支持时返回退出码 3，并输出 `{"error":"riff_invalid"}`。

读取始终按块进行：`riff.WavReader.iter_frames()` 默认每块最多 65,536 帧，帧以 `float32` 返回；分析不会把整段音频读入内存。

## 检测流程

1. `framing.scan_envelopes` 逐块去直流，按 10 ms 分帧，计算每帧 RMS 与峰值包络；立体声逐帧取两路较大值。
2. 每 30 秒窗口取 15 分位帧能量作为底噪，并在窗口中心之间线性插值。
3. 高阈值为 `max(底噪 + 12 dB, -50 dBFS)`，可用 `--noise-margin` 修改 12 dB；低阈值比高阈值低 3 dB，用于迟滞维持。
4. 默认 250 ms hangover，短促停顿不会把一句话切成两段；可用 `--hangover-ms` 调整。
5. `refine` 回读原始采样，在检测边界附近重新做 10 ms 能量帧，从首个超阈帧向静音方向回溯/前探。相邻帧需单调且高于局部底噪 3 dB，最多回扩 `--max-fade-ms`（默认 600 ms），同时合并 `--merge-gap-ms` 内的短静音。

全静音输出 0 段且静音采样数等于总采样数；全发声输出 1 段且静音为 0；首尾静音计入总静音。

## 命令

`analyze` 输出单行 JSON，包含：

```json
{
  "sample_rate": 96000,
  "channels": 2,
  "encoding": "pcm32",
  "channel_mode": "stereo",
  "total_samples": 1991040,
  "segment_count": 5,
  "segments": [
    {"start_sample": 78720, "end_sample": 392640, "fade_in_ms": 0, "fade_out_ms": 10}
  ],
  "silence_total_samples": 540480,
  "silence_total_ms": 5630
}
```

批处理命令：

```bash
python -m audioscan batch enqueue data/fixtures --db .audioscan/audioscan.db
python -m audioscan batch run --workers 4 --db .audioscan/audioscan.db
python -m audioscan export --out data/cuts --format csv
python -m audioscan report --summary --json
```

`batch enqueue` 对目录内非符号链接 `.wav` 文件做流式 SHA-256；相同内容只入队一次。`batch run` 的父进程唯一写 SQLite，工作进程只做分析；中断后重新运行会把遗留 `running` 作业恢复为 `pending`，已完成项不会重复处理。

`export --format csv` 生成 `cuts.csv`，列固定为：

```text
file,index,start_sample,end_sample,start_ms,end_ms
```

`report` 默认同时写出：

- `.audioscan/reports/summary.json`
- `.audioscan/reports/summary.csv`
- `.audioscan/reports/files.json`
- `.audioscan/reports/files.csv`

## 数据库

SQLite 使用 WAL、`busy_timeout=1000` 和事务重试。写冲突最多尝试 5 次；仍失败时返回退出码 4。

四张表：

- `files`：内容哈希、路径、格式、总采样数、状态、错误。
- `jobs`：每文件唯一作业、`pending/running/done/failed` 状态、尝试次数。
- `segments`：逐段采样点、毫秒边界与渐入渐出。
- `config`：最近一次检测参数。

## 合成夹具与自检

`synth` 使用固定 seed 和文件序号生成确定性 WAV，覆盖 PCM16/24/32、float32、单/双声道、22.05 kHz 到 96 kHz，并写入已知语音区间与总静音答案。夹具包含漂移底噪、150 到 250 ms 渐入渐出、0.4 秒以上停顿和一路极弱声道的立体声场景。

`selfcheck` 在临时目录重新生成 12 个夹具，使用与分析、批处理相同的检测器逐文件比对段数；静音总采样数误差不得超过一个 10 ms 帧。任何不符返回 6。

## 退出码

| 码 | 含义 |
|---|---|
| 0 | 全部成功 |
| 2 | 参数错误 |
| 3 | WAV 解码失败或短于一帧 |
| 4 | SQLite 锁冲突重试耗尽 |
| 5 | 批量部分文件失败，报告已写出 |
| 6 | 自检段数或静音长度不符 |

## 测试

```bash
python -m pytest
```

测试覆盖容器格式、损坏文件、自适应底噪、迟滞/hangover、渐入渐出上限、短静音合并、全静音/全发声、立体声降级、SQLite 幂等与重试、批处理失败隔离、清单导出和完整 CLI 验收链。
