# relaycheck

区域直播分发边缘节点的可复用媒体处理内核：统一有理数时间基准、解析/裁决
MP4 与 MXF 元数据、RTP 去重排序与带宽适配、音频重采样/声道/响度、
WAL 事务式恢复，以及独立的哈希与关键帧核验。全部本地、确定性，无外部服务。

- Go 指令固定 `go 1.25.6`
- MP4：`github.com/abema/go-mp4 v1.7.3`
- RTP：`github.com/pion/rtp v1.10.6`

## 数据流

```
元数据解析与裁决(meta) -> 共享有理数时钟(media) -> RTP 重排/适配(transport)
 -> 音频转换/响度(audio) -> WAL + 原子提交(txn) -> 独立核验(verify)
```

`cmd/relaycheck` 只负责解析参数并调用 `internal/pipeline`。

## 常用命令

```bash
go mod download
go mod tidy
go build ./...
go test ./...

# 完整验收（构建二进制并校验 0/2/75 退出码与续跑哈希一致）
bash scripts/acceptance.sh
```

正常演示：

```bash
go run ./cmd/relaycheck demo --seed 25 --target-rate 48000 --layout stereo \
  --integrated-lufs -18 --duplicate-every 17 --reorder-window 9 \
  --burst-loss 6 --bandwidth-kbps 12000,4000,9000 --out ./.relay-ok
```

损坏输入：

```bash
go run ./cmd/relaycheck demo --case truncated-mp4 --out ./.relay-bad   # 语义退出码 2
```

注入中断与续跑：

```bash
go run ./cmd/relaycheck demo --crash-after-segment 3 --out ./.relay-recover            # 语义退出码 75
go run ./cmd/relaycheck demo --crash-after-segment 3 --out ./.relay-recover --resume   # wal_replay=1 ...
```

## 退出码

| 码 | 含义 |
|----|------|
| 0  | 成功 |
| 2  | 输入无效（解析、时间基准、旋转/裁剪/色彩、空声道、削波、区间不完整等） |
| 3  | 完整性不可恢复（校验和 / WAL / 清单） |
| 75 | 注入中断（可 `--resume`） |

> 注意：标准 `go run` 会把子进程的非零退出统一包装成 shell 码 `1`，但会打印
> `exit status 2/75`；程序自身的结构化报告里也有 `exit_code=2/75`。需要由进程
> 退出码精确断言时，请使用 `go build -o bin/relaycheck ./cmd/relaycheck` 后的
> 二进制（`scripts/acceptance.sh` 已据此校验）。

## 约束

- 单轨内存缓冲上限 64 MiB；MP4 损坏长度、超大样本与整数溢出均安全拒绝。
- MXF 仅实现样例使用的 KLV 子集；未知标签默认原样保留，严格模式显式报错。
- 不解码视频画面、不访问真实采集设备、不连接外部服务；夹具全部由 Go 生成。
- `.relay-*/`、`bin/`、`*.test` 等产物不进入仓库。
