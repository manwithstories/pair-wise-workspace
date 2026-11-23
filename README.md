# archive — 影视资料馆归档迁移流水线

从旧存储迁移采访/纪录片母版时，恢复时间线并重建可随机访问的索引。
纯终端工具，无图形界面，无外部服务依赖；样例与夹具全部由 Go 代码生成。

## 构建与验收

```sh
go mod tidy && go build ./... && go test ./...
go run ./cmd/archivectl demo --seed 42 --memory-mib 16 --consumer-delay-ms 8 --loss-burst 5
go run ./cmd/archivectl demo --fixture truncated --seed 42
```

成功路径输出 `av_sync_error_ms=0.8`、`loss_recovery_rate=0.80`、
`p95_latency_ms<=120`、`peak_memory_mib<=16`、`index_entries`、
`blocked_node=none`、`index_sha256`，退出码 0。
截断夹具输出 `truncated_offset`、`blocked_node=index`、`index_entries=0`，
进程退出码 2（`go run` 会打印 `exit status 2` 并以 1 退出——这是
`go run` 自身的行为；直接运行构建出的二进制可观察到退出码 2）。

## 退出码

| 码 | 含义 |
|----|------|
| 0  | 成功 |
| 2  | 输入损坏或不可恢复依赖（截断媒体、非法清单、下游提前关闭等） |
| 1  | 内部错误 |

## 架构

| 包 | 职责 |
|----|------|
| `internal/manifest` | 素材清单解析；负时长/缺轨/越界编辑列表返回带轨道与偏移的错误 |
| `internal/timeline` | 统一轨道/编辑列表/节目时间换算（误差 <1ms，溢出可定位）；修复倒退、断档、非零起点，保留原始值与修复决策，不可唯一判定区间标记为不可定位 |
| `internal/index` | 按解码时间流式构建稀疏关键帧索引（含展示时间、字节偏移、前置解码点）；分段校验后原子提交，幂等键防重复追加 |
| `internal/stream` | 有界缓冲管道（背压、取消/写失败/下游关闭均可定位释放）；RTP 序号重排与窗口内丢包恢复 |
| `internal/scheduler` | 依赖 DAG 调度，报告首个阻塞节点与依赖链；幂等键=输入摘要+参数+节点版本，仅可重入节点重试，续跑复用已成功节点 |
| `cmd/archivectl` | 验收入口：确定性夹具生成（go-mp4 写入/解析）、DAG 装配、指标输出 |

## 约定

- 仅支持 MP4/MOV 输入；索引落本地文件，临时分段校验后原子 rename。
- 内存受 `--memory-mib` 约束：管道字节上限来自该预算，慢消费者触发背压，
  不会缓存全量帧或包。
- 相同输入重复执行产生相同 `index_sha256`。
- 延迟指标由本机生成的定长（1200B）RTP 序列测得；丢包恢复率 =
  重排窗口内恢复包数 / 注入丢包数；同步误差基于修复后轨道时间戳。
