# mediapack

将比赛录制素材（MP4/MOV）离线转换为移动网络低延迟互动回放所需的分片清单。
纯终端工具，仅做本地解封装、分片、表示层对齐与清单校验；不负责转码与网络传输。

## 构建与测试

```sh
go mod tidy
go build -o /tmp/mediapack ./cmd/mediapack
go test ./...
```

## 命令

```sh
# 生成内置样例（多码率有效素材 + 截断/缺表/损坏变体）
mediapack fixture --dir /tmp/mediapack-fixtures

# 探测轨道、时长、时间基准与索引完整度
mediapack inspect --input valid.mov

# 切片并对齐多码率表示层，输出清单
mediapack build --input valid.mov --out out --segment 2s \
  --checkpoint-samples 128 --target-bitrates 450000,900000 \
  --tolerance 80ms --policy strict

# 从最后提交的检查点续跑（指纹或配置不一致时拒绝，退出码 4）
mediapack build --input truncated.mov --out bad --resume --policy skip
mediapack resume --input truncated.mov --out bad --policy skip   # 等价写法

# 重新解封装并校验清单与输入一致
mediapack verify --input valid.mov --manifest out/manifest.json
```

省略 `--target-bitrates` 时，各表示层以实测码率为基准（偏差为 0）。

## 退出码

| 码 | 含义 |
|----|------|
| 0  | 完成且通过校验 |
| 2  | 输入或参数错误（含清单与媒体不一致） |
| 3  | 存在不可恢复媒体错误 |
| 4  | 检查点与输入/配置不匹配，拒绝续跑 |

## 架构

数据流：容器解封装 → 统一时间轴 → 分片与表示层对齐 → 索引与清单 → 校验报告。

- `internal/clock` — 统一时间基准（90000 Hz  canonical ticks），RTP 时间戳辅助校验（pion/rtp）
- `internal/fault` — 可恢复 / 可跳过 / 致命三级错误模型与 strict/skip 策略
- `internal/demux` — MP4/MOV 解封装（go-mp4）：轨道、样本时间戳、索引完整度；
  缺 stss 时扫描 AVC IDR 重建关键帧索引，无法确认的区间标为不可信
- `internal/segment` — 切点必须落在可用视频随机访问点；音频按统一时间轴归属，互不重叠
- `internal/ladder` — 多码率轨道按统一切点对齐，容差（默认 80ms）外报告缺档区间
- `internal/index` — 每固定样本数原子写检查点；续跑前校验输入指纹与配置哈希
- `internal/manifest` — 临时文件 + 原子替换 + 完成标记；重复运行摘要一致
- `internal/metrics` — 目标/实际码率偏差、样本质量统计、缺损区间、对齐误差
- `internal/fixture` — 内置样例生成（有效 / 截断 / 缺表 / 损坏），无外部文件依赖
- `internal/pipeline` — build / verify 编排
- `cmd/mediapack` — CLI 入口与退出码映射

## 错误处理约定

- 所有异常记录文件偏移与媒体时间；跳过策略不掩盖缺口，缺损区间始终出现在报告中。
- 严格策略（默认）遇到可跳过级错误即以退出码 3 中止；skip 策略跳过坏样本并继续。
- 清单与检查点均通过临时文件 + 原子替换写入；启动时清理未提交的临时结果。
- 不执行输入容器中的外部引用，不覆盖源素材（输出目录不得为输入所在目录）。
