# kmsctl — 本地分片加密、授权与校验流水线

纯本地、单进程、不监听端口的内容保护工具：把密钥轮换、许可证签发与
分片校验串成可重复执行的流水线，并输出可核对的量化结果。仅依赖
Go 1.22+ 标准库，无 cgo、无网络访问。

## 构建

```sh
go build ./...
```

## 快速开始

```sh
go run ./cmd/kmsctl gen-fixture --shards 24 --shard-size 65536 --seed 7 --out testdata/fx
go run ./cmd/kmsctl encrypt --fixture testdata/fx --rotate-every 8 --out testdata/pack
go run ./cmd/kmsctl issue --pack testdata/pack --ttl 90s --shards 0-23 --out testdata/lic.json
go run ./cmd/kmsctl verify --pack testdata/pack --license testdata/lic.json --json
# {"shards":24,"rotations":2,"key_ids":{"0":"kv1","8":"kv2","16":"kv3"},"failed":0,"fail_reasons":{}}
```

完整演示（含篡改、nonce 重放、许可证过期三类事故）：

```sh
./scripts/demo.sh
```

## 子命令

- `gen-fixture`：按固定种子确定性生成分片、`manifest.json` 与主密钥
  材料 `master.key`（样例数据全部由代码生成）。
- `encrypt`：逐片经进程内分发器租用密钥版本，AES-256-GCM 加密；
  `--rotate-every N` 按片数轮换，`--rotate-window D` 按时间窗轮换，
  `--grace D` 设置旧版本宽限期，`--quota N` 设置分发器租约配额。
  产物：`shards/` 密文、`manifest.json`、`keys.json`（版本表）、
  `meta.json`（片→版本映射）、`master.key`。
- `issue`：签发许可证，含版本、分片区间、`not_before`/`not_after`、
  nonce、机器指纹与 HMAC 签名。`--nonce` 可指定 nonce（演示重放用），
  `--ttl` 接受负值（演示过期用），`--machine` 可覆盖机器指纹。
- `verify`：校验链依次检查签名、时间窗、区间、版本状态与重放，
  随后并发逐片解密；部分分片失败不中断，最终汇总总数与失败数。
- `report`：汇总包内分片数、轮换次数、版本状态机与轮换边界。

## 安全设计

- 每片 AAD 绑定清单摘要，篡改任意字节定位到具体分片（`tampered`）。
- GCM nonce 由 `key_id`、分片序号与轮换 epoch 派生，绝不复用。
- 许可证签名密钥由主密钥在内存中派生，从不落盘；日志不输出密钥明文。
- 重放窗口持久化在 `pack/replay.json`，窗口内条目绝不淘汰，并发安全。
- 旧密钥版本在 grace 期内仍可解密；许可证过期（`expired`）与密钥
  过期（`key_expired`）分开上报。

## 失败原因码

`bad_signature`、`clock_skew`、`expired`、`out_of_range`、
`version_retired`、`key_expired`、`version_rollback`、`replay`、
`tampered`、`machine_mismatch`。

## 退出码

- `0` 全部通过
- `1` 存在校验失败
- `2` 参数或 IO 错误
- `3` 内部状态不一致（如租约配额耗尽 `quota_exhausted`、重放缓存满）
