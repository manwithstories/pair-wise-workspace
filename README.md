# dashpack

面向多语种导览点播的 DASH 清单打包/校验工具：将 360p–1080p 多档访问副本的
分片元数据归一化到公共时间网格，产出切点对齐的 MPD，并可反向展开清单与
磁盘分片对账。纯终端、纯本地、无外部服务依赖；只处理分片元数据，不做编解码。

## 构建与运行

```sh
go run ./cmd/dashpack pack --reps 4 --duration 600 --seg 4 --seed 42 \
    --out out/manifest.mpd --report out/report.json
go run ./cmd/dashpack verify --mpd out/manifest.mpd --json
```

`pack` 在 `--in` 为空时按 `--seed/--reps/--duration/--seg` 生成确定性合成
分片（各档 timescale、起始偏移、码率不同，r4 中途切换 timescale），
否则探测 `--in` 目录树。重复 pack 输出字节一致；已有输出内容不同则需
`--force` 才重写。`--addr number|time` 选择 `$Number$` 或 `$Time$` 寻址。

## 退出码

| 码 | 含义 | 结构化 error_code |
|----|------|-------------------|
| 0 | 成功 | — |
| 2 | 参数错误 / 输出已存在 / 溢出 | `PARAM_ERROR` `OUTPUT_EXISTS` `OVERFLOW` |
| 3 | 对齐超容差 / 时间戳回退 / 时长不符 | `ALIGNMENT_EXCEEDED` `TIMESTAMP_REGRESSION` `DURATION_MISMATCH` |
| 4 | 缺分片 / 分片数不符 / 分片损坏 | `MISSING_SEGMENT` `SEGMENT_COUNT_MISMATCH` `SEGMENT_CORRUPT` |
| 5 | 清单解析失败 / XML 往返不等 | `MANIFEST_PARSE_ERROR` `XML_ROUNDTRIP_MISMATCH` |

失败时 stderr 输出单行 JSON 结构化错误；`verify --json` 的报表照常输出。

## 报表字段

`representations`、`segments_per_rep`、`expand_first`/`expand_last`（首个
Representation 展开的首末分片名）、`max_deviation_ms`（清单时间线与磁盘
分片的最大偏差，含起点与时长）、`drift_count`（磁盘时间戳回退段数）、
`tolerance_ms`（默认 10，`--tolerance-ms` 可调）。

## 设计要点

- 时间累计全程 int64：分片边界先按各自 timescale 换算到公共时钟
  （90000），再吸附到最近的公共切点网格，溢出即报错。
- 每档一个 AdaptationSet，Representation 按码率升序；`presentationTimeOffset`
  非零、`startNumber` 非 1、中途 timescale 变化均逐段重算。
- 清单模型由 pack 与 verify 共用（`internal/mpd`），写出前做
  marshal→parse→marshal 字节级往返校验。
- 所有输出经临时文件 + rename 原子落盘。

## 目录

```
cmd/dashpack/main.go     子命令与退出码
internal/mpd/            清单模型、时间线、占位符展开
internal/ingest/         分片探测与确定性合成
internal/align/          公共原点与切点吸附
internal/pack/           清单构建与原子写
internal/verify/         展开对账与偏差统计
internal/errs/           结构化错误码
```
