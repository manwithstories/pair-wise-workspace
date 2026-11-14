# tsprobe

MPEG-TS 节目入库前探测工具：节目表重建、版本统计、连续计数审计、按字节切片与夹具校验。纯终端形态，无图形界面依赖。

## 构建

```sh
go mod tidy
go build ./cmd/tsprobe
```

## 子命令

```sh
# 生成确定性样例码流（按种子合成，注入丢包与 CC 跳变）
tsprobe gen --out testdata/mux3.ts --seed 20261115 --programs 3 --seconds 30 \
    --pat-versions 4 --inject-loss 47 --inject-cc-jump 6

# 扫描：PAT/PMT 重建、版本计数、CC 审计、PID 映射表
tsprobe scan --in testdata/mux3.ts --json out/report.json

# 切片：按累计字节切段，每段头部重注入当前 PAT/PMT（CC 从零重编）
tsprobe slice --in testdata/mux3.ts --out-dir out/slices \
    --gop-bytes 1048576 --manifest out/slices/manifest.json

# 校验：复扫比对报告；--seed 重建期望计数；--manifest 校验切片；
# 内置 go-astits 独立复解，比对 PID 集合与包计数
tsprobe verify --in testdata/mux3.ts --report out/report.json \
    [--seed 20261115 --inject-loss 47 --inject-cc-jump 6] \
    [--manifest out/slices/manifest.json]
```

## 退出码

| 码 | 含义 |
|----|------|
| 0  | 全部通过 |
| 1  | 参数或 IO 失败 |
| 2  | 前 1024 包内无合法 PAT |
| 3  | 计数与期望不符 |
| 4  | manifest 与实际段不符 |

## 结构

- `cmd/tsprobe` — 子命令、参数与退出码
- `internal/tspkt` — 188/192 字节对齐、0x47 重同步、adaptation field 与 payload 提取
- `internal/psi` — 自研 MPEG-2 CRC32、跨包段重组、PAT/PMT 解析、版本状态机
- `internal/cc` — 逐 PID 连续计数与 discontinuity_indicator 处理
- `internal/report` — PID 映射表、JSON 与文本报告
- `internal/slicer` — 复用 PSI registry 切段并写 manifest
- `internal/fixture` — 按种子合成多节目流并注入丢包与跳变

约束：顺序读流、不整流入内存、只读输入文件；`section_length` 与描述子长度均有上限与越界保护；不解析 PES 负载。
