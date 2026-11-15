# dashkit — OTT 点播自研 MPD 打包与校验工具

纯本地、无外部服务依赖。从合成转码分片元数据生成自研 MPD
（SegmentTemplate + SegmentTimeline），并发展开、跨档对齐校验与数值报告。

## 构建

```sh
go build -mod=vendor ./...
```

## 数据流

fixture → model → template → timeline → mpd → packager → report

`internal/model` 是唯一共享依赖（清单模型 + big.Rat 时基），其余包单向引用，
`cmd/dashkit` 只做编排。

## 子命令

```sh
# 1. 生成 7 档（5 视频 + 2 音频）分片元数据夹具
go run ./cmd/dashkit fixture --out testdata/segments.json --periods 2 --gop-ms 2002 --duration-ms 120000

# 2. 注入阶梯并生成 MPD + 分片索引（out/index/）
go run ./cmd/dashkit build --segments testdata/segments.json \
  --ladder "240p:300k,360p:800k,480p:1200k,720p:2500k,1080p:5000k;aac:64k,128k" \
  --timescale 90000 --start-number 1 --pto 0 --out out/manifest.mpd

# 3. 并发展开全部表示层的分片 URL（首末各 3 条打印）
go run ./cmd/dashkit expand --mpd out/manifest.mpd --json out/expand.json

# 4. 投影到 1/1000 时基做跨档对齐校验
go run ./cmd/dashkit verify --mpd out/manifest.mpd --tolerance-ms 11 --json out/report.json
```

## 异常演练

`fixture --anomaly missing|regression|zerodur|periodorder|offset` 注入缺失分片、
时间倒退、@d=0、Period 乱序、单档 5ms 偏移；verify 逐条打印错误码
（E_MISSING_SEGMENT / E_TIME_REGRESSION / E_ZERO_DURATION / E_PERIOD_ORDER 等），
其余档仍出部分报告。

## 退出码

- 0 全部对齐
- 2 参数或清单非法（ID 重复、bandwidth≤0、timescale=0、模板与 SegmentList 并存等）
- 3 跨档偏差超 `--tolerance-ms`（打印最差三处定位）
- 4 输入损坏但已出部分报告

## 测试

```sh
go test ./...        # 单元测试
go test -race ./...  # 并发展开/共享报告竞态覆盖
```
