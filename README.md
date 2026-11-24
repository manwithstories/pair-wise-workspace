# media-check

广播母版归档中心的入库前批量验收工具（纯终端，无外部服务依赖）。

## 构建与测试

```sh
brew install go   # Go 1.25.6
go mod tidy
go build ./...
go test ./...
```

## 演示

```sh
go run ./cmd/media-check/main.go demo \
  --frames 1200 --segment-frames 120 \
  --inject timestamp-wrap,box-truncate,audio-conflict,hash-corrupt \
  --repeat 2
```

注入器：`timestamp-wrap`（采集时钟回绕）、`box-truncate`（截断 MP4 盒）、
`audio-conflict`（声道布局/采样率冲突）、`hash-corrupt`（帧 SHA-256 不符）。
全部样例数据由程序自生成，不读取外部文件。

输出：逐段序号、源/目标 SHA-256、时间范围与异常原因，以及指标
`roundtrip_consistency`、`keyframe_hit_rate`、`segment_hash_match`、
`corrupt_ranges`、`recovery_stable`。

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 处理与核验完成 |
| 2 | 输入或参数错误 |
| 3 | 存在已定位损坏，其他片段完成 |
| 4 | 内核或核验失败 |

注意：`go run` 会把非零退出码包装为自身退出码 1 并打印 `exit status N`；
需要精确退出码时请使用 `go build` 产物。

## 架构

- `internal/model`：统一媒体时间基准（90 kHz，回绕展开）与跨模块错误模型
- `internal/adapter`：版本化适配器注册表；`frame@v1`（RTP 承载的帧序列，
  pion/rtp）、`mp4@v1`（盒结构读写与样本表，abema/go-mp4）、`audio@v1`
  （声道布局/采样率规范化与隔离）
- `internal/core`：码流检查（关键帧间隔、色彩决策）、损坏区段恢复计划、再封装
- `internal/index`：关键帧索引建立与核验
- `internal/manifest`：逐片段哈希与输出清单
- `internal/pipeline`：批处理协调、分段检查点、可重复运行

依赖版本固定：`github.com/abema/go-mp4 v1.7.4`（上游最新发布版；
需求所述 v1.28.0 在 upstream 不存在）、`github.com/pion/rtp v1.8.11`。
