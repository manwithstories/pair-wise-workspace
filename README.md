# Turncode 离线转码批次工具

纯 Go、离线运行的 8-bit YUV420p 转台视频转码与质量回归工具。工具自带确定性素材生成、GOP 级码率控制、I/P 编解码、可恢复检查点和分段 PSNR 报告，不调用外部编码器或网络服务。

## 构建与测试

```sh
go mod vendor
go build ./...
go test ./... -race
```

依赖仅为 `gonum.org/v1/gonum v0.15.x`，并已放入 `vendor/`。代码不使用 cgo。

## 端到端示例

```sh
go run ./cmd/transcode fixture --out testdata/turntable \
  --width 320 --height 180 --fps 30 --frames 600 --scene mixed --seed 42

go run ./cmd/transcode run --spec testdata/turntable/manifest.json \
  --target-bps 1200000 --gop 30 --segment-frames 60 --work work/a --fail-after-gop 6
# exit 3: 已在完整 GOP 边界暂停

go run ./cmd/transcode resume --work work/a
go run ./cmd/transcode report --work work/a --json

go run ./cmd/transcode run --spec testdata/turntable/manifest.json \
  --target-bps 1200000 --gop 30 --segment-frames 60 --work work/b --qp-offset 4
go run ./cmd/transcode report --work work/b --baseline work/a/report.json
# exit 4: 检测到质量回归，并列出退化段
```

注意：`go run` 在子进程非零退出时会打印 `exit status N`，但 Go 工具自身仍返回 1。自动化验收退出码 3/4/5 时请先 `go build -o bin/transcode ./cmd/transcode`，再直接运行该二进制；程序本身的退出码按上表定义。

`fixture` 的宽高必须为偶数。manifest 记录 `frame_bytes`、`file_bytes` 和 SHA-256；相同 seed 和参数生成相同文件。`mixed` 包含缓慢转台、渐变、颗粒噪点、高光与硬切。

## 退出码

| 码 | 含义 |
|---|---|
| 0 | 完成且无质量回归 |
| 1 | 参数、路径或 IO 错误 |
| 2 | 输入规格、预算或 manifest 大小校验失败 |
| 3 | 在 GOP 边界中断，可续跑 |
| 4 | 相对 baseline 检测到质量回归 |
| 5 | 非法状态迁移、检查点损坏且无法回退 |

## 工作目录

```text
work/<name>/
  run.json                 # 冻结的规格、源路径和 QP offset
  state.json / .bak        # 带版本与 SHA-256 的状态记录
  checkpoints/
    gop-000000.json ...    # 每个完整 GOP 的偏移、QP、VBV、PSNR 聚合
    checkpoint.json        # 最新的原子指针
  bitstream.bin            # 逐帧长度前缀的基础码流
  report.json              # 终端报告同步写出的机器可读版本
```

检查点写入顺序为临时文件、`fsync`、`rename`、目录 `fsync`。续跑会扫描完整 GOP 记录；若码流尾部截断或最新检查点损坏，则回退到最后一个可验证且位流仍存在的 GOP，并截断多余尾部。

## 码控与质量

- GOP 预算同时考虑目标码率和 VBV 累积偏差；QP 始终限制在 1–51。
- 冷启动先做比例探测，随后使用 `gonum/stat` 对 `log(bits)` 与 QP 在线线性回归。
- 一个 GOP 内可用相邻整数 QP 逐帧混合，以消除整数 QP 造成的阶梯偏差；每帧 QP 都写入帧头。
- 场景硬切会重置局部 QP 探测范围，但保留全片码率历史。
- PSNR 对所有 Y/U/V 平面逐帧计算，只保留段级累加量，因此内存不随帧数增长。
- 均值下降超过 0.5 dB，或段均值标准差增加超过 0.3 dB，即判为回归。

`--qp-offset` 是用于质量对比的粗粒度偏移，内部按编解码器的量化档位映射；对比组不参与 baseline 的码控反馈，避免反馈把偏移重新拉回 baseline 质量。当前组仍会写出 `report.json`，便于连续批次比较。

## 安全与边界

- 只接受 8-bit YUV420p，无音频、无封装。
- 宽 16–1920、高 16–1080 且必须为偶数；最多 108000 帧。
- 拒绝 `..` 路径片段和路径链上的符号链接；manifest 的 `yuv_file` 必须是纯文件名。
- 工作池最多 16 个并发作业；单作业内 GOP 严格串行。`--fail-after-gop` 和 `--cancel` 只在 GOP 提交后生效。
