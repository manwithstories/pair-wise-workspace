# avpipe

本地、无外部服务依赖的低延迟音视频整形与 RTP 传输流水线。Go 1.25.6，纯终端形态。

## 架构

```
internal/
  media/     90 kHz 公共时钟、统一帧/音频块模型、可定位错误模型、安全整数运算
  adapter/   版本化帧容器解析器(v1)、MP4 侧车索引(go-mp4)、确定性样本生成器
  audio/     分数累加重采样(48k->16k)、声道下混(mono/stereo/5.1->stereo)、LUFS 响度
  rtp/       丢包/突发/抖动模拟、有界抖动缓冲(重排+窗口缺口)、分组 XOR FEC(pion/rtp)
  pipeline/  端到端引擎，串联各子系统并传播坏帧缺口，强制队列/缓存上限与取消
  metrics/   指标汇总、P95 与验收阈值评估(退出码 0/2/3)
cmd/avpipe/ 命令行入口
scripts/    demo.sh(验收演示) 与 verify.sh(vet+race+build)
```

## 关键不变量

- 未知容器/索引版本显式失败；索引缺失时按连续 PTS 推导并标记 `derived` 置信度。
- 截断只保留完整记录；校验失败/PTS 回退记为时间缺口，绝不静默改写时间线。
- 48k->16k 重采样用分数累加器，长流累计采样误差不超过 1 个样本。
- RTP 超出等待窗口输出缺口并继续；单包丢失可由分组 XOR 重建，连续多丢记为不可恢复。
- 坏帧缺口同时传播到音频对齐与 RTP 指标。
- 相同种子重复运行产生相同 `output_sha256` 与指标。
- 解析输入有长度上限，所有偏移读取经溢出与越界检查；600 帧内存占用低于 64 MiB。

## 运行

```sh
go mod download
go build -o /tmp/avpipe ./cmd/avpipe
go test ./...

/tmp/avpipe demo --frames 600 --seed 7 --loss 0.05 --burst 2 \
    --truncate 3 --corrupt-every 97
/tmp/avpipe demo --frames 600 --seed 7 --loss 0 --case clean
```

输出字段：`frames`、`recovered_pct`、`sync_max_ms`、`latency_p95_ms`、
`bad_records`、`index_fallback`、`output_sha256`（另附诊断计数）。

退出码：`0` 指标通过；`2` 输入不可恢复；`3` 指标未达标。

## 依赖

- github.com/abema/go-mp4 v1.7.4
- github.com/pion/rtp v1.10.6
