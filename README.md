# mp4probe

轨道交通车载录像离线取证工具：纯本地、可脚本串接的 MP4 索引与关键帧定位批处理器。
只解析容器与样本表，不做解码、编码、渲染；mdat 仅按 offset 段读。

## 构建与测试

```sh
go mod tidy && go mod vendor
go test ./...
```

## 子命令

```sh
# 生成离线夹具（normal/editlist/wrap/co64/moovtail）
go run ./cmd/mp4probe fixture --out work/fixtures --cases normal,editlist,wrap,co64,moovtail --seed 20261115
# 建索引：stdout 打印摘要，--out 落盘完整索引
go run ./cmd/mp4probe index --in work/fixtures/wrap.mp4 --out work/index.json --json
# 关键帧定位 + 逐帧 SHA-256
go run ./cmd/mp4probe seek --in work/fixtures/editlist.mp4 --at 00:07:31.240 --track 1 --frames 8 --json
# 区间清单导出
go run ./cmd/mp4probe export --in work/fixtures/normal.mp4 --from 00:00:10.000 --to 00:00:12.000 --track 1 --out work/samples.json
# 哈希复核
go run ./cmd/mp4probe verify --in work/fixtures/normal.mp4 --index work/index.json --expect work/samples.json
```

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 成功 |
| 2 | 参数错误（缺参、时间码非法、track 不存在、路径越界） |
| 3 | box 结构错误（截断、表不自洽、缺 moov/stbl、损坏 elst） |
| 4 | 索引自洽校验失败 |
| 5 | 无可定位关键帧或越界（早于首 sync、晚于 duration、stss 空、单样本/零时长轨） |
| 6 | 哈希不一致 |
| 7 | 读写错误 |

## 结构

- `cmd/mp4probe`：子命令分发与退出码
- `internal/cli`：参数、时间码、JSON 输出（stdout/落盘）
- `internal/box`：自研 box 头遍历（32/64 位 size、size=0、uuid、深度上限），索引与导出共用
- `internal/moov`：moov/trak/stbl 各表解析（stsd/stts/ctts/stss/stsc/stsz/stz2/stco/co64/elst）
- `internal/timeline`：elst 段映射（空编辑、负 media_time、rate≠1、tkhd 冲突）与 32 位 DTS 回绕展开
- `internal/index`：按轨并行建索引、落盘回读与自洽校验
- `internal/seek`：stss 二分、解码路径、逐帧 SHA-256
- `internal/export`：区间清单导出与哈希复核
- `internal/fixture`：5 类离线夹具合成

依赖 `github.com/abema/go-mp4`（版本见 go.mod）仅取 box 常量；box 头遍历为自研实现。
