# MediaLab

移动多人协作场景下的弱网音视频元数据、RTP 恢复与 A/V 同步实验台。全部素材由 Go 程序确定性生成，不读取仓库外媒体、不访问真实网络或设备。

## 一键验收

```bash
go mod tidy
go build ./...
go test ./...
go run ./cmd/medialab demo --seed 17 --burst-loss 7 --bandwidth-kbps 900,280,1200
```

期望关键输出：

```text
roundtrip_consistency=100.00%
keyframe_index_hit=100.00%
segment_sha256=12/12
duplicate_dropped=2
reordered_packets=3
burst_recovered=7/7
av_skew_ms=12
first_error_offset=21
demo_status=all_expected_paths_completed
```

相同参数两次运行的 `segment_sha256_XX=...` 与 `keyframe_index_sha256` 完全一致。可运行：

```bash
./scripts/verify_demo.sh
```

该脚本执行构建、测试、两次输出 diff、哈希比较、参数错误退出码 2 和截断 KLV 安全拒绝退出码 3 的检查。

## 生成夹具

```bash
./scripts/gen_fixtures.sh fixtures/generated
# 或
go run ./cmd/medialab gen-fixtures --out fixtures/generated
```

生成：

- `normal.mp4`：视频/音频轨道、合法旋转、裁剪、stss 关键帧和色彩冲突；
- `crop_out_of_bounds.mp4`：越界裁剪；
- `rotation_illegal.mp4`：45 度非法旋转；
- `normal.mxf`：KLV 轨道元数据；
- `truncated_klv.mxf`：BER 长度仍声明 8 字节但值被截断，`first_error_offset=21`。

检查容器：

```bash
go run ./cmd/medialab inspect --file fixtures/generated/normal.mp4
go run ./cmd/medialab inspect --file fixtures/generated/truncated_klv.mxf
```

## 退出码

编译后的 `medialab` 二进制遵循：

| 码 | 含义 |
|---:|---|
| 0 | 全部预期生成、拒绝、隔离、恢复和验证路径完成 |
| 2 | 命令参数无效 |
| 3 | 输入被安全拒绝，例如损坏/截断容器 |
| 1 | 未预期内部错误或验收不变量失败 |

注意：Go 工具的 `go run` 会把子进程的任意非零退出统一包装为父进程退出码 1，并打印 `exit status N`；脚本因此先 `go build -o ... ./cmd/medialab`，再直接验证二进制的 2/3 退出码。成功路径下题目给出的 `go run ... demo ...` 仍为退出码 0。

## 架构与数据流

```text
MP4 box / MXF KLV
  -> internal/format        带绝对原始偏移的受限解析
  -> internal/metadata      explicit_override > track > container 色彩裁决
  -> internal/pipeline      合法旋转/裁剪规范化，坏轨道隔离
  -> internal/index         stss/KLV 关键帧索引

RTP 包
  -> internal/rtp.ReorderBuffer  去重、冲突隔离、有界乱序、16/32 位回绕
  -> internal/rtp.Engine         限时重传优先，XOR 单缺失恢复
  -> internal/rtp.AdaptiveBuffer 带宽阶梯目标，硬上限 120ms

索引 + 恢复片段
  -> internal/sync          有理数时间轴、漂移校正、有界静音、等待已验证关键帧
  -> internal/report        逐片段 SHA-256、命中率和验收指标
  -> cmd/medialab           仅做参数/输出/退出码适配
```

`internal/clock.Rational` 是唯一有理数时间基准；`internal/model.Fault` 携带 `stage`、`track`、原始字节 `offset`，恢复结果使用 `model.RecoveryState` 和 `model.SegmentRef` 跨模块保留来源与状态。

## 已实现并自测的格式子集

- MP4：`ftyp/moov/trak/tkhd/mdia/mdhd/hdlr/minf/stbl/stss/colr/crop/rott` 及容器 box 递归；32/64 位 size 和长度越界检查。
- MXF：16 字节 UL 风格 key、短/长形式 definite BER length，以及轨道 ID、handler、时间尺度、尺寸、旋转、裁剪、NCLX 色彩和关键帧列表。
- RTP：使用 `github.com/pion/rtp v1.10.6` 完成包结构和 marshal/unmarshal；恢复逻辑在本地模拟，不使用网络。

队列、输入长度、重排窗口、已见序号、隔离区、重传缓存、NACK、XOR 组、自适应队列和关键帧索引均设有上限。项目不解码/解密真实媒体 essence，不访问加密轨道、真实网络或设备。
