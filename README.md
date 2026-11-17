# hdrcheck

纯本地 GARC 媒体容器 HDR 静态元数据解析与一致性校验工具。从自研 GARC 容器中
解析母版显示信息（ST 2086 等价集合）与内容亮度（MaxCLL/MaxFALL），合并容器
TLV 与视频轨 HDR-SEI 两个来源，输出可审计的不一致清单。**不调用 FFmpeg 或任何
外部服务，不读取真实媒体，所有样例由 `fixture` 子命令本地生成。**

## 构建

需要 Go 1.22+（在 1.25 上验证），仅依赖 cobra v1.8.1：

```bash
go build ./...
go test ./...
```

## 演示流程

```bash
# 1. 生成全部本地夹具（正常值/继承/缺字段/冲突/截断/坏校验/重复/未知版本/重叠/长度不符）
go run ./cmd/hdrcheck fixture --out ./fixtures

# 2. 解析混合场景并写稳定 JSON 报告
go run ./cmd/hdrcheck inspect --input ./fixtures/hdr-mixed.garc --json ./out/inspect.json

# 3. 严格模式一致性校验
go run ./cmd/hdrcheck validate --input ./fixtures/hdr-mixed.garc --strict
```

> 注：`go run` 会把程序的非零退出码统一呈现为 1。要观察真实退出码，请先
> `go build -o hdrcheck ./cmd/hdrcheck` 再直接运行二进制。

## 子命令

| 命令 | 作用 |
|---|---|
| `inspect --input <f> [--json <p>]` | 解析并展示字段来源/出处，审计模式不阻断 |
| `validate --input <f> [--strict]` | 跨来源、跨轨一致性校验；默认审计模式 |
| `fixture --out <dir>` | 在目录下生成全部 GARC 测试夹具 |

## 字段来源与合并

每个字段标注四态来源：`container`（元数据区 TLV）、`sei`（视频轨补充信息）、
`default`、`missing`。合并规则：

- 容器缺省、SEI 存在 → 继承，状态 `inherited`；
- 两路都存在且相等 → `ok`，以 container 为准、SEI 佐证；
- 两路都存在但不等 → `conflict`，**双值均保留，禁止默认值覆盖**；
- 两路均无 → `missing`，绝不静默写零。

## 退出码契约

| 码 | 含义 |
|---|---|
| 0 | 无问题，或审计模式下仅含 warning |
| 2 | 业务冲突/缺字段（仅 `--strict` 下） |
| 3 | 容器损坏或参数错误（截断、未知版本、校验和、越界、重复、重叠等） |
| 4 | 内部异常 |

## 安全边界

- 单文件硬上限 512 MiB；所有区间读取带边界检查，长度/偏移做 uint32 溢出校验。
- LEB128 限 uint32 域，超长/不终止分别报 `VARINT_OVERFLOW`/`TRUNCATED_RECORD`。
- 输出只含输入文件名（剥离目录）与 sha256，不读取环境变量，不泄露路径或令牌。
- 容器与 SEI 记录均带 IEEE CRC32，校验失败的记录丢弃且不参与合并。

字节布局与全部错误代码见 [`docs/garc-format.md`](docs/garc-format.md)。

## 目录结构

```
cmd/hdrcheck/          cobra 入口：inspect/validate/fixture
internal/garc/         容器头、轨道表、边界读取、CRC、LEB128
internal/hdr/          统一 HDR 模型、容器 TLV 解析、SEI 解析
internal/validate/     来源合并、缺省继承、跨轨一致性、冲突分级
internal/report/       稳定排序 JSON 与终端摘要
internal/fixture/      GARC 字节构建器与场景生成器
internal/app/          流水线编排与退出码裁决
tests/                 全链路集成测试
docs/                  GARC 格式规范
```
