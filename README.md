# mediarun

现场演出结束后的多机位素材交付流水线：把波动链路上的 RTP 回传、ISO BMFF
媒体与定时元数据、以及可复跑的时间线合成，收拢成一条纯本地、可核验的终端
工具。

全部样例数据由 Go 代码生成，不读取任何外部文件；不依赖网络或外部服务。

## 环境

- Go 1.25.6（`go.mod` 的 `go` 与 `toolchain` 均为 `1.25.6`）
- macOS / Linux 终端均可，纯 CLI，无图形依赖
- 固定依赖：
  - `github.com/abema/go-mp4 v1.3.0` —— ISO BMFF 读写
  - `github.com/pion/rtp v1.8.11` —— RTP 包解析

```sh
go mod tidy
go build ./...
go test ./...
```

## 验收命令

```sh
go run ./cmd/mediarun demo --seed 42 --packet-loss burst --bandwidth 2,8,1 --out ./artifacts
go run ./cmd/mediarun verify --dir ./artifacts
```

正常路径会打印：

```
roundtrip_rate=1.000
keyframe_hit_rate=1.000
segments=4
detected_gaps=5
rollback_count=1
recovered=1
committed_hash_match=1
segment_hashes[0]=<64 位十六进制 SHA-256>
...
```

损坏路径（截断盒、非法长度、突发丢包、提交中断）会打印
`detected_gaps>0`、`rollback_count>0`、`corrupt_rejected=corrupt_cases`；
恢复后打印 `recovered=1` 与 `committed_hash_match=1`。

退出码：

| 码 | 含义 |
| --- | --- |
| 0 | 全部断言通过 |
| 1 | 核验不一致（哈希、缺口或计数） |
| 2 | 参数或输入格式错误 |

`--packet-loss` 可取 `clean`、`reorder`、`duplicate`、`burst`、`wrap`；
只有 `burst` 必须产生可检出的缺口，其余场景允许零缺口。

产物目录内容（均在 `--out` 指定目录内）：

```
manifest.json      已提交片段清单（原子替换写入）
manifest.jsonl     离屏复核帧清单
seg-NNN.json       每个片段的规范化事件与缺口
wal.jsonl          写前日志
transport.json     传输阶段摘要
corrupt.json       损坏输入自检结果
frames/            离屏复核 PNG 序列
```

## 结构

```
cmd/mediarun          验收命令适配（demo / verify，退出码 0/1/2）
internal/clock        统一 90 kHz 整数刻度、换算、溢出与 32 位回绕
internal/model        跨轨事件词汇、稳定排序与错误分类
internal/rtp          RTP 序号去重、有界乱序窗口、缺口登记
internal/ingest       接收校验、带宽自适应批次、落盘缓冲
internal/parse        ISO BMFF 盒扫描/读写，字幕/章节/ID3/emsg/SEI
internal/index        关键帧索引构建、查询校验、损坏重建
internal/timeline     跨轨合并与关键帧分段
internal/render       确定性离屏帧序列与清单
internal/txn          WAL、原子清单提交、恢复重放
internal/verify       逐片段 SHA-256 与独立回读核验
internal/testkit      种子驱动的媒体、RTP 缺陷流与损坏盒生成
internal/pipeline     三套子系统的端到端编排
```

## 三条子系统与不变式

**传输**：`ingest.Stream` 校验每个 RTP 包，去重并只在窗口内按序交付。
窗口溢出时登记缺口并跳到最近的可交付序号，绝不凭空补包；迟到包单独计数。
带宽变化调整批次与内存积压上限，超出部分先追加写入落盘日志再离开内存，
重放顺序与连续运行一致。

**解析与索引**：`parse` 严格校验盒长度、截断与嵌套深度，未知盒保留原始
字节并在写回时逐字节还原。`index` 的每次查询都用样本表核对目标时间、
时长、偏移与长度；结构或内容失配时由样本表重建，命中率按确定性查询集统计。

**合成与事务**：`timeline` 以 `(start, kind, track, sequence)` 稳定排序，
只在关键帧处切段，同时刻事件不跨段。`txn` 对每个片段先写 WAL 再原子替换
清单；清单写入前的崩溃由恢复重放补齐，写入后的崩溃保留原哈希。已提交片段
的哈希在任何后续失败中都不得变化——`verify` 独立重读文件证明这一点。

## 资源约束

- 单轨样本数上限 4096，RTP 重排窗口上限 4096，复核帧上限 512 帧
- 复核栅格上限 320×180，媒体文件上限 64 MiB，盒深度上限 12
- 这些上限超限时返回显式错误（分类为 `limit exceeded`），不会静默截断
