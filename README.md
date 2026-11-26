# mediascan

本地多码率 DASH / HLS（含 discontinuity）与 fMP4 素材的验收工具：清单往返解析、
有理数秒统一时间线、fMP4 结构与关键帧分析、逐片段 SHA-256、原子检查点续跑、
确定性溯源报告。纯终端形态，无网络与外部服务依赖。

## 构建与测试

```sh
go version          # go1.25.6
go mod tidy && go build ./... && go test ./...
```

## 使用

```sh
# 生成可重复样例（两档 DASH、含两处 discontinuity 的 HLS、20 个 fMP4 片段）
go run ./cmd/mediascan fixture --out .tmp/clean

# 扫描：索引构建（含清单往返与时间线映射）+ 有界并发片段分析
go run ./cmd/mediascan scan --input .tmp/clean --state .tmp/state --hashes

# 注入故障的样例与可续跑中断
go run ./cmd/mediascan fixture --out .tmp/bad --fault truncated-box
go run ./cmd/mediascan scan --input .tmp/bad --state .tmp/bad-state --stop-after 3
go run ./cmd/mediascan scan --input .tmp/bad --state .tmp/bad-state --resume --hashes
```

故障种类：`truncated-box`、`missing-segment`、`invalid-duration`、
`tfdt-rollback`、`discontinuity-boundary`。

## 退出码

| 码  | 含义                         |
| --- | ---------------------------- |
| 0   | 完成且无损坏                 |
| 1   | 输入或配置错误               |
| 2   | 报告完成但发现损坏           |
| 75  | 可续跑中断（`--stop-after`） |

> 注意：`go run`（go1.25.6）不透传子进程退出码——程序非零退出时它打印
> `exit status N` 但自身以 1 退出。需要核验退出码时请使用构建产物：
>
> ```sh
> go build -o .tmp/mediascan ./cmd/mediascan
> .tmp/mediascan scan --input .tmp/bad --state .tmp/bad-state --stop-after 3 || test $? -eq 75
> ```

## 架构与数据流

```
清单解析 (internal/manifest)
  → 统一时间线与摘要索引 (internal/index, 仅扫描阶段构建)
  → 有界并发片段读取 (internal/pipeline, 并发 ≤16)
  → 结构 / 关键帧 / 哈希分析 (internal/segment)
  → 原子检查点 (pipeline/checkpoint, 写临时文件 + rename)
  → 溯源报告 (internal/provenance, report.json + report_sha256)
```

- `internal/core`：有理数秒时基（`Rat`）与共享错误区间（`Range`）。
  时间线错误进入索引；片段错误进入关键帧结果与溯源记录。
- 索引失效：输入摘要（全部输入文件的 SHA-256 链）或分析配置摘要变化时，
  存储的索引与检查点自动重建。
- 检查点状态机：片段只有 `pending` / `processing` / 终态（`done` / `failed`）
  三种状态；中断遗留的 `processing` 在加载时重置为 `pending`。
- 确定性：输出按索引顺序排序，摘要不依赖并发度；重复运行
  `report_sha256` 不变、`new_processed=0`。
- 安全约束：仅本地静态文件；拒绝远程地址（`://`、`//`）、路径越界
  （`..`、绝对路径）、非常规文件与超限输入（清单 ≤1MiB、片段 ≤64MiB、
  清单 ≤64 个、片段 ≤4096 个）。
