# audio_pipeline

纯 Python 的提示音资产批处理工具：分块读取、任意有理率重采样、BS.1770-4
响度测量、4 倍真峰值保护、归一化、削波审计与可复算报告。运行期只依赖
NumPy、soundfile 与 pytest，不调用 scipy/librosa/ffmpeg/sox/子进程。

## 快速开始

```bash
python tools/make_fixtures.py fixtures
python -m audio_pipeline.cli batch \
  --in fixtures --out out --target-rate 48000 --target-lufs -16 \
  --block 4096 --report out/report.json
python -m audio_pipeline.cli verify --report out/report.json --dir out
python -m pytest -q
```

`batch` 输出 48 kHz、双声道、24 位 WAV，逐文件打印一行摘要，最后打印
`total=... ok=... failed=...`。增加 `--resume` 可复用已有报告中参数一致且
输出仍存在的成功记录；每处理完一个文件都会原子更新检查点报告。

## 模块

- `kernel.py`：约分 L/M、Kaiser 窗多相 FIR、`0.92 * min(1, M/L)` 截止、
  逐相位直流归一，并以参数指纹缓存内核。
- `resampler.py`：跨块保存输出相位和输入余量，flush 时以零填充到目标长度，
  按中心群延迟对齐，不把滤波器尾部泄漏成额外样本。
- `io_block.py`：soundfile 分块读写、交织帧缓冲的 stride 声道视图，统一
  float64；PCM_U8/PCM_16/PCM_24 由 libsndfile 正确缩放。
- `loudness.py`：48 kHz 及任意采样率适配的 K 加权、400 ms 块、100 ms 跳步、
  绝对/相对双门限。
- `truepeak.py`：4 倍多相 sinc 过采样真峰值；削波按 `|x| >= 0.999` 与连续
  三样本以上平台取并集，状态跨块延续。
- `normalize.py`：目标 LUFS、+12 dB 增益上限、-1 dBTP 上限；超限回退最多
  5 轮并记录每轮增益和预测峰值。真峰值安全优先于响度目标。
- `pipeline.py`：第一遍重采样并写 `.cache/` float64 内存映射，第二遍施增益
  写最终 WAV，两遍均分块统计。
- `job.py`：扫描分组、共享内核、断点复用、单文件失败隔离、报告落盘。
- `verify.py`：反向读取交付 WAV，重算 LUFS/RMS/真峰值/削波并核对参数和指纹。

## report.json

每条成功记录固定包含：

`in_rate`、`out_rate`、`out_channels`、`rms_dbfs_before`、
`rms_dbfs_after`、`lufs_before`、`lufs_after`、`true_peak_dbtp`、
`clipped_before`、`clipped_after`、`kernel_fingerprint`。

另有 `input`、`output`、`status`、`error_code`、`error`、`gain_db`、
`target_lufs` 和 `normalization_rounds`。报告不含时间戳，同参数重跑除
耗时外内容一致。不可表示的静音指标固定写成 `-200.0`，保证合法 JSON。

## 退出码与文件错误码

- CLI `0`：全部成功；`1`：部分或全部文件失败；`2`：参数/路径非法；
  `3`：verify 复算超差；`4`：内核比值或抽头越界。
- 文件记录 `10`：空音频；`11`：损坏/不可解码；`13`：输出不可写；
  `14`：不支持的声道布局；`15`：处理异常。
- 单文件失败只记录该文件，批量继续；空文件、损坏文件和只读输出均可定位。

## 内存与精度

音频主体只在内存映射临时文件中落盘，重采样与测量均按 `--block` 分块。
卷积窗口分片进一步限制临时数组；默认 4096 帧时峰值工作集远低于 64 MB。
中间音频全部 float64；重采样参考误差目标为 `1e-9`，报告浮点保留 6 位。
夹具完全由 `tools/make_fixtures.py` 确定性合成，不引用外部音频。
