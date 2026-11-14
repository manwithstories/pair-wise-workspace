# tsaudit

面向赛事直播收录文件的离线 MPEG-TS 体检工具。实现是纯终端程序，不启动窗口、浏览器、摄像头或麦克风，也不访问外部服务。

## 构建与测试

要求 Go 1.22 或更高版本。依赖已固定到 `vendor/`，构建默认使用 vendor 目录：

```sh
go build ./...
go test ./...
GOPROXY=off go build -mod=vendor ./...
```

`go-astits v1.8.0` 只用于 TS 包级序列化和底层流处理；PAT/PMT 段重组、CRC32-MPEG2、描述符和版本账本均为本仓库自研实现。

## 验收命令

```sh
go run ./cmd/tsaudit fixtures gen \
  --out testdata/a.ts --truth testdata/a.truth.json \
  --seed 42 --programs 2 --pat-versions 4 --pmt-versions 5 \
  --drop 7 --cc-jump 3

go run ./cmd/tsaudit scan --in testdata/a.ts --json --out out/scan.json

go run ./cmd/tsaudit slice --in testdata/a.ts --program 1 \
  --segment-ms 2000 --out-dir out/seg --index out/index.json

go run ./cmd/tsaudit verify \
  --truth testdata/a.truth.json --report out/scan.json
```

默认夹具为约 4.8 万包、12 秒、两套节目；同时包含 PAT/PMT 多版本、PMT/ES PID 重映射、跨包 PMT、CRC 损坏、卫星式丢包、CC 跳变、孤立 PID 和零包 ES。生成结果和扫描/切片产物均被 `.gitignore` 排除。

## 命令与退出码

- `fixtures gen`：按种子生成故障 TS 与真值 JSON，可调 `--duration-ms`、`--crc-errors` 等参数。
- `scan`：流式输出表版本、PID 映射、连续计数、PCR、CRC 与孤儿信息。`--json` 输出完整 JSON；`--out` 写文件；`--strict` 在发现丢包时返回 2。
- `slice`：按节目、固定 PCR 栅格和换表点切段；每段首包为重新生成的 PAT，随后为 PMT。
- `verify`：比较 truth 与 scan 报告；一致返回 0，首个差异打印字段后返回 3。
- 参数错误或本地 I/O 错误返回 1；命令行帮助返回 0。

注意：`go run` 会把子进程的非零退出码统一显示为自身的退出码 1；需要直接检查 2/3 时请先 `go build -o tsaudit ./cmd/tsaudit`，再运行该二进制。

## JSON 关键字段

- `packets_total`、`sync_losses`、`crc_errors`：包级定界与段损伤证据。
- `pat.version_changes`、`pat.versions`：PAT 版本变化数和每版首现包号。
- `pmt[].program`、`pmt[].version_changes`、`pmt[].streams`：节目级 PMT 账本与 ES 循环。
- `pid_map[]`：PID、节目号、流类型、语言、注册标识、包数、负载字节和首末包号；`orphan` 标记真正未声明过的 PID。
- `continuity.loss_packets`、`cc_jumps`、`duplicates`、`discontinuities`：连续计数状态机结果。
- `pcr[]`：PCR 计数、回绕、间隔均值/极值和抖动。
- `zero_packet_es`、`orphan_pmts`、`orphan_pids`：零包 ES 与表引用异常单列。

## 数据流

```text
file -> 188/192 packet reader -> PID demux -> PSI section reassembly
     -> PAT/PMT/descriptor parsers -> versioned table cache + PID snapshot
     -> continuity/PCR audit -> JSON report
                         \-> slicer (same PSI parser, two-pass streaming)
```

包读取器逐字节重捕同步，默认不缓存整文件；切片器采用两遍流式扫描，避免大文件常驻内存。本机缓存读场景实测 522MB / 291 万包扫描约 0.2 秒，`GODEBUG=gctrace=1` 显示 GC 堆维持在 3–4MB，明显高于 80MB/s 吞吐要求且远低于 200MB 内存上限。
