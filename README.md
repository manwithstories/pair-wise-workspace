# tcr — 分段码率控制转码与质量基线工具

纯终端、纯本地的纪实节目分发档转码工具：自研 8x8 DCT + 量化 + 自适应范围编码
容器格式，按段做 QP 二分/割线搜索码率控制，逐段 PSNR 沉淀为可比较基线。

## 构建

```sh
go mod tidy   # 拉取 gonum.org/v1/gonum v0.15.0
go build ./cmd/tcr
```

## 使用

```sh
# 1. 生成合成母版（渐变/字幕板/噪点/摇镜/硬切/混合 六段）
go run ./cmd/tcr fixture --out testdata/masters --segs 6 --w 320 --h 240 --frames 48

# 2. 按段目标码率编码（4 worker，每段完成即写检查点）
go run ./cmd/tcr encode --manifest testdata/masters/manifest.json --target-kbps 900 --out out/a --workers 4

# 3. 逐段 PSNR + 码率偏差，写 JSON 基线
go run ./cmd/tcr metrics --manifest testdata/masters/manifest.json --dec out/a --json out/a/metrics.json

# 4. 对基线做回归判定
go run ./cmd/tcr regress --baseline out/base/metrics.json --current out/a/metrics.json --max-psnr-drop 0.5 --max-seg-std 2.0
```

## 断点续跑

```sh
go run ./cmd/tcr encode --manifest testdata/masters/manifest.json --target-kbps 900 --out out/a --workers 4 --fail-after-segs 2
# 退出码 5；state.json 记录已完成的 2 段
go run ./cmd/tcr encode --manifest testdata/masters/manifest.json --target-kbps 900 --resume out/a/state.json
# 从第 3 段补齐，报告与单线程一致
```

## 退出码

| 码 | 含义 |
|----|------|
| 0  | 成功 |
| 1  | 参数/输入错误（奇数宽高、零帧、段越界、清单缺失等） |
| 2  | 质量回归（均值下降或段间波动超阈值） |
| 3  | 码率不可达（输出 infeasible 与建议值） |
| 4  | 检查点状态损坏（state_corrupt） |
| 5  | 注入失败（--fail-after-segs） |

注：经 `go run` 运行时，Go 工具链会把非零退出码打印为 `exit status N` 并以 1 退出；
直接运行编译产物可拿到原始退出码。

## 布局

- `cmd/tcr` 子命令解析、参数校验、退出码映射
- `internal/fixture` 合成 YUV420p 母版与清单生成
- `internal/yuv` 帧与平面读写、尺寸校验
- `internal/codec` DCT/量化、RLE+范围编码、容器封装与解码
- `internal/rc` 按段目标码率的 QP 搜索（gonum 拟合 bits-QP 曲线 + 割线反馈）
- `internal/task` 任务状态机与日志式检查点
- `internal/sched` 分段并发调度与失败注入
- `internal/metrics` 逐段 PSNR 与统计
- `internal/report` JSON 报告与基线回归判定
