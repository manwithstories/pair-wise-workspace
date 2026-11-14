# dashpack — 赛事点播回看本地 DASH 打包与对齐校验工具

纯终端、仅标准库（Go 1.22），不访问网络。扫描上游转码农场输出的分片化
fMP4，生成 MPD 清单与模板，并校验跨码率档的时间对齐。

## 子命令

```sh
go build ./...
go run ./cmd/dashpack gen --out testdata/fixtures --seed 42   # 生成四档夹具
go run ./cmd/dashpack scan --dir testdata/fixtures/r1         # 扫描单档时间线
go run ./cmd/dashpack pack --root testdata/fixtures --out out # 写 manifest.mpd + index.json
go run ./cmd/dashpack expand --mpd out/manifest.mpd --root testdata/fixtures
go run ./cmd/dashpack expand --template 'seg-$RepresentationID$-$Number%05d$.m4s' --id r1 --number 7
go run ./cmd/dashpack verify --mpd out/manifest.mpd --root testdata/fixtures --json
```

## 退出码

- `0` 通过
- `2` 参数或清单非法（含模板未知变量、格式宽度非法，报错带行列）
- `3` 跨档偏差超过阈值（默认 2ms，`--threshold-ms` 可调）
- `4` 读盘失败（缺 fixtures.json、分片不可读、展开目标不存在等）

## 工作方式

- `scan` 解析 mvhd/trex/mfhd/tfhd/tfdt/trun，以 tfdt 基值加 trun 累计
  时长求每片起止；缺 tfdt 时按累计值推导。
- `align` 以最高码率档为基准，把各档首片 tfdt 写入
  `presentationTimeOffset`，统一换算毫秒后逐边界比对偏差。
- `pack` 复用同一模型与模板展开器，按码率稳定排序，原子落盘，
  重复运行字节一致；任一步失败不写半成品。
- `verify` 复读清单并输出 JSON 报告：分片缺口、重叠、末片过短、
  序号非单调、模板展开计数与最大/平均偏差。

## 夹具

`gen` 按种子生成四档（timescale 1000/12800/90000/90000），各档首片
起点不同，边界带亚毫秒正弦抖动；r2 缺第 17 片（序号缺口），r3 第 30
片缺 tfdt，各档末片为短尾。媒体分片以 `$Time$`（tfdt 基值）命名，
使清单在序号缺口下仍可寻址。
