# mediac — 广播素材批量检查与再封装工具

区域广播机构的播出工程师工具：从带时间戳的 MP4/MOV 生成分片清单，
对齐 2–4 个码率表示层，支持中断续跑与交付校验。纯终端形态，仅处
理本地文件，不调用外部转码器。

## 构建与验收

```sh
go mod tidy && go build ./... && go test ./...
go run ./cmd/mediac demo   --out /tmp/media-demo
go run ./cmd/mediac resume --out /tmp/media-demo --interrupt-after 2
go run ./cmd/mediac verify --out /tmp/media-demo
```

`demo` 自生成全部夹具（正常 3 码率 + 截断 moov / 损坏 mdat / 非单
调时间戳 / 未知编解码四类异常样例），输出 `damaged_offset_bytes`、
`damaged_time_ms`、`segments`、`boundary_max_delta_ms`、`sha256`。
`resume` 模拟中断后从原子检查点续跑，输出 `resume_ms`；已完成分
片不重复写入，重复运行清单与 SHA-256 完全一致。`verify` 扇入比较
各表示层分片起止时间并重算哈希。

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 成功 |
| 2 | 输入损坏（含未知编解码结构，显式失败） |
| 3 | 扇入边界校验失败（偏差超过一帧） |
| 4 | 检查点源摘要或配置摘要不匹配，拒绝续跑 |

注意：`go run` 会把子进程的非零退出码折叠为 1（Go 工具链行为），
精确退出码请使用 `go build -o mediac ./cmd/mediac` 后的二进制，
或观察 `go run` 打印的 `exit status N`。

## 架构

```
internal/clock     统一时间基准：精确有理数运算，禁止静默舍入
internal/model     可序列化错误模型（跨模块传播，映射退出码）
internal/probe     go-mp4 解析容器与轨道，定位首个损坏 box
internal/normalize 声道/采样率/色彩元数据差异记录与归一化
internal/segment   关键帧+音频采样边界共同切点；确定性清单
internal/fanout    按统一切点分发各码率任务（RTP 90kHz 时间戳）
internal/recovery  原子检查点；源/配置摘要变化即拒绝续跑
internal/verify    扇入边界比较与输出哈希校验
internal/fixture   自生成确定性 MP4 夹具（不引用外部文件）
cmd/mediac         演示入口（demo/resume/verify/probe）
```

约束：单文件上限 20 GB，表示层 2–4 路，未知编解码结构显式失败。
损坏定位误差不超过一个视频帧；恢复耗时远低于 1000 ms；边界差不
超过一帧。
