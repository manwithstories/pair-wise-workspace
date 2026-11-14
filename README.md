# kmsctl — 离线课件加密分发与授权校验工具

纯本地、仅依赖 Go 标准库的企业内训课件保护工具。课件按固定大小切片后用
AES-256-GCM 逐段加密，密钥按「段数 + 虚拟墙钟」双触发轮换；许可证绑定设备、
段范围与有效期；重放账本落盘，进程重启后仍能识别重放。

## 构建

```
go build ./...
```

## 快速演示

```
./scripts/demo.sh
```

或手动执行：

```
MK=00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff
go run ./cmd/kmsctl gen-fixture --out fixtures/lesson.bin --size 2097152 --device dev-A
go run ./cmd/kmsctl encrypt --in fixtures/lesson.bin --out out/lesson.enc \
  --manifest out/lesson.json --segment-size 131072 --rotate-every 4 --master-key $MK
go run ./cmd/kmsctl license issue --device dev-A --segments 0-15 \
  --not-before 2026-01-01T00:00:00Z --not-after 2026-01-01T00:10:00Z --out out/lic.json
go run ./cmd/kmsctl verify --manifest out/lesson.json --license out/lic.json \
  --device dev-A --now 2026-01-01T00:05:00Z --report out/report.json
```

成功 stdout 形如：

```
{"rotation_count":4,"segment_count":16,"segments":[{"index":0,"key_id":"k-0001",...}],"failures":[...]}
```

## 模块

| 包 | 职责 |
| --- | --- |
| `internal/keyschedule` | 双触发轮换，稳定 epoch/key_id/密钥派生，确定性虚拟时钟 |
| `internal/keydist` | 进程内分发，一次性 TTL 凭据、越界/超时判定、发放台账 |
| `internal/cryptox` | AES-256-GCM 分段加解密，nonce 派生，AAD 绑定 |
| `internal/manifest` | 清单读写与 HMAC 完整性校验 |
| `internal/license` | 许可证签发/校验，设备与有效期绑定，时钟偏移容忍 |
| `internal/replay` | 单调序号 + nonce 账本，原子落盘，跨进程重放识别 |
| `internal/audit` | 轮换次数、逐段 key_id、失败原因、按 key 覆盖统计 |
| `cmd/kmsctl` | 子命令入口、参数解析、退出码映射、确定性夹具生成 |

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 成功 |
| 2 | 参数错误 |
| 3 | 完整性失败：`MANIFEST_TAMPER` / `AAD_MISMATCH` / `TAG_INVALID` |
| 4 | 许可证时效：`EXPIRED` / `NOT_YET_VALID` |
| 5 | 设备绑定或重放：`DEVICE_MISMATCH` / `LICENSE_SIGNATURE_INVALID` / `REPLAY` |
| 6 | 授权范围或凭据：`OUT_OF_SCOPE` / `KEY_EXPIRED` / `CREDENTIAL_ALREADY_USED` |

## 关键设计

- **nonce 唯一**：12 字节 = 4 字节 epoch + 8 字节段号，同 epoch 下段号唯一。
- **AAD 绑定**：段号 + key_id + 清单头摘要，段换位或密钥错配即 `AAD_MISMATCH`。
- **确定性**：相同主密钥与参数（含输出文件名，文件名进入清单摘要）重复运行，
  密文、清单与统计字节级一致；夹具由固定种子 PRNG 生成。
- **流式处理**：按段读取/`ReadAt`，常驻缓冲不超过段大小 + 64KiB，256MB 输入
  远低于 32MB 内存上限。
- **密钥不落盘**：主密钥仅以 SHA-256 指纹（`master_key_id`）出现在清单，
  各用途子密钥经 HMAC 按 label 派生。
- **时钟偏移**：严格「超出」`--max-skew` 窗口才判失效（默认 60 秒）。
