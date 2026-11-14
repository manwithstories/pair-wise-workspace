# rcx — 车机端教学视频编码质控工具

纯终端工具链：确定性夹具生成、分片规划、两遍码率控制编码、断点续跑、回归门禁。

## 子命令

```sh
# 1. 生成确定性 YUV420 夹具（gradient|texture|motion|noise）
go run ./cmd/rcx gen --scene motion --seed 7 --frames 100 --width 320 --height 240 --out fixtures/a.yuv

# 2. 按帧率与段时长切片，规划每段目标比特（cbr|capped）
go run ./cmd/rcx plan --input fixtures/a.yuv --fps 25 --segment 2 --bitrate 900k --mode capped --out plan.json

# 3. 两遍码率控制编码：第一遍拟合 R=a·Q^-b（gonum 最小二乘），第二遍反解 QP；
#    每段完成即写原子检查点，重复执行自动从首个未完成段续跑
go run ./cmd/rcx encode --plan plan.json --workers 4 --checkpoint .rcx/a.state --out out/a

# 4. 聚合报告并与基线逐段回归比对，--json 落盘
go run ./cmd/rcx verify --run out/a --baseline out/base --json out/a.json
```

## 退出码

| 码 | 含义 |
|----|------|
| 0  | 通过 |
| 2  | 质量回归（PSNR 或码率差超阈值） |
| 3  | 任务失败，检查点已保留可续跑 |
| 4  | 参数或状态非法 |

## 报告字段

`target_kbps`、`actual_kbps`、`deviation_pct`、`psnr_mean_db`、`psnr_seg_std_db`、
`vbv_min_ratio`、`segments_total`、`verdict`，另含逐段 QP/比特/PSNR 与逐帧 PSNR。

## 设计要点

- `internal/fixture`：种子驱动的确定性 YUV420 帧序列，四类场景。
- `internal/codec`：8x8 DCT + JPEG 量化表 + zigzag 游程 + 指数哥伦布比特计数；QP 为 0.25 步进浮点。
- `internal/rc`：log-log 域最小二乘拟合 R=a·Q^-b，反解 QP；段内 VBV 模拟（初始半满、匀速排出）。
- `internal/job`：六态状态机（非法迁移写事件日志）、原子检查点（tmp+rename，半写回退 .bak）、并发段调度。
- `internal/report`：终端表格 + JSON，逐段 PSNR/码率差回归判定。

## 测试

```sh
go test ./...
```
