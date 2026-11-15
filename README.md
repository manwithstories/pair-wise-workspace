# Loudness delivery checker

纯 Go 标准库实现的本地响度与真峰值检查工具，面向游戏宣传片、实机演示和赛事集锦的交付验收。读取 WAV 时只保留固定大小的样本块和 400 ms 块统计，支持最长 500 MB 的单文件流式分析。

## 构建与测试

```sh
go build ./...
go test ./...
```

## 生成夹具

夹具全部由 `internal/gen` 合成，不依赖仓库外素材：

```sh
go run ./scripts/genfixtures fixtures
```

`fixtures/` 中固定包含 5 个合规文件、2 个不合规文件和 1 个损坏文件，便于复现批处理计数。

## 使用

```sh
go run ./cmd/loudness selftest
go run ./cmd/loudness measure --in fixtures/sine_1k_stereo_48k.wav --target -23 --ceiling -1 --json
go run ./cmd/loudness measure --in fixtures/quiet_lead_in.wav --target -23
go run ./cmd/loudness batch --dir fixtures --jobs 4
```

`measure` 默认输出 `key=value` 终端文本，`--json` 输出单行 JSON；`-o FILE` 将结果落盘。`batch` 默认输出对齐表格，末行为 `summary total=... pass=... fail=... error=...`。

判定使用目标响度 ±0.5 LU 的容差，真峰值不得超过 `--ceiling`。全静音返回 `reason=silent`，不足 400 ms 返回 `reason=too_short`；无块通过门控时 integrated 记 `-inf`，JSON 中以 `null` 表示。

退出码：`0` 全部合规，`1` 存在不合规项，`2` 解码失败或格式/采样率不支持，`3` 参数用法错误。批处理中单文件失败只影响该行，存在任一错误行时整体退出 `2`。

需要精确核对退出码时请先构建二进制（`go run` 对非零子进程通常只返回 1）：

```sh
go build -o loudness ./cmd/loudness
./loudness batch --dir fixtures --jobs 4
```

## 实现要点

- RIFF 块扫描支持乱序块、奇数填充、`WAVE_FORMAT_EXTENSIBLE` 和 `0xFFFFFFFF` 流式 data 长度。
- K 加权系数在 32 kHz 至 192 kHz 范围内按采样率推导并校验极点稳定性。
- 400 ms 门控块按 100 ms 跳步（75% 重叠），短时窗为 3 s；门控依次执行 -70 LUFS 绝对门限和 -10 LU 相对门限。
- 真峰值采用 4 倍多相 FIR 过采样；四分之一采样率相位正弦的读数为 `+3.01 dBTP`。
- 批处理按文件名排序后提交 worker 池，结果按输入顺序输出；每个 worker 独占可变滤波器与缓冲区。
