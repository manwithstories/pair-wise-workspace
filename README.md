# 影视资料馆归档媒体处理内核

从旧制作工程中恢复多轨时间线、剪辑点与转场片段的本地媒体处理内核。
纯本地运行、无外部服务依赖，可嵌入归档流水线，完成片段切分、静音检测、
内容指纹去重，并为每个结果保留来源与恢复记录。

## 技术栈

- Go 1.25.6（`go.mod` 固定 `go 1.25.6`，无 toolchain 指令）
- `github.com/asticode/go-astiav v0.43.0`（CGO 绑定 FFmpeg n9.0 的
  libavformat / libavcodec / libavutil / libswresample / libswscale）
- 纯终端形态，无图形界面、无浏览器、无设备权限申请

> 说明：本机 Homebrew 的 `ffmpeg` 为 9.1，其库 SONAME 与 n9.0 主版本一致，
> go-astiav v0.43.0 可正常链接运行；容器/编解码不兼容时按 `unsupported` 隔离。

## 目录结构

```
cmd/archive/        验收命令适配器（demo / resume / verify）
internal/
  model/            有理时基、统一微秒 MediaTime、可分类 MediaError
  adapter/          版本注册、能力协商；arc1 纯 Go 容器与 astiav 真实容器
  timeline/         多轨异构时基归一到单一微秒时间轴
  cut/              剪辑点吸附取整、转场复合段（不重复计时）
  audio/            采样窗 RMS 静音检测、跨块合并、缺帧不完整标记
  fingerprint/      内容+时间跨度联合指纹、去重分组、冲突候选保留
  journal/          幂等键、检查点两阶段提交、溯源、路径安全
  manifest/         原子清单（临时文件 + fsync + rename）
  verify/           码率 / PSNR / RMS / 对齐核验与门限判定
  fixture/          自生成五类 arc1 样例 + 真实 FFV1+PCM/MKV 样例
  pipeline/         串联各子系统的可中断、可续跑编排
scripts/archive     透传语义退出码的启动器（见“退出码”说明）
```

## 安装与构建

```bash
brew install go@1.25 ffmpeg pkg-config
export PATH="$(brew --prefix go@1.25)/bin:$PATH"
export PKG_CONFIG_PATH="$(brew --prefix ffmpeg)/lib/pkgconfig"

go mod tidy
go build ./...
go test ./...
```

## 演示与验收

样例数据全部由代码在 `--dir` 指定目录内生成，不引用任何外部文件，包括：
正常双轨、截断尾包、时间戳回绕、非法剪辑点、重复转场，以及一个真实 MKV。

```bash
# 处理 2 个片段后模拟中断（可续跑）
go run ./cmd/archive demo   --dir "$TMPDIR/archive-demo" --preset all --interrupt-after 2
# 从中断点续跑，幂等短路已提交片段
go run ./cmd/archive resume --dir "$TMPDIR/archive-demo"
# 只读已原子提交的清单重算核验指标
go run ./cmd/archive verify --dir "$TMPDIR/archive-demo" --max-bitrate-delta 8 --max-align-ms 1
```

成功样例的量化门限：码率偏差 ≤ 8%、PSNR ≥ 32 dB、清单对齐误差 ≤ 1 ms。
续跑前后清单与片段文件的 sha256 保持一致；损坏样例报告已恢复片段数与
首个损坏时间（演示中为 200000µs，保留 0–160ms 的已验证前缀）。

`verify` 额外支持 `--min-psnr-db`（默认 32）。

## 退出码

| 码 | 含义 |
|----|------|
| 0  | 通过 |
| 2  | 参数或输入无效 |
| 3  | 模拟中断，已提交片段完好、可续跑 |
| 4  | 不可恢复媒体错误（格式/编解码不兼容、无法保留前缀的损坏） |
| 5  | 量化门限失败 |

### 关于 `go run` 与退出码

Go 工具链的 `go run` 在子进程以非零码退出时，**自身固定返回 1** 并打印
`exit status N`，不会透传应用的语义退出码（3/4/5）。这是 `go run` 的固有
行为，应用层无法改变。

需要在 shell 编排中获得真实退出码时，使用随仓库提供的透传启动器（内部先
`go build` 再直接执行二进制，退出码原样透传）：

```bash
scripts/archive demo   --dir "$TMPDIR/archive-demo" --preset all --interrupt-after 2  # -> 3
scripts/archive resume --dir "$TMPDIR/archive-demo"                                     # -> 0
scripts/archive verify --dir "$TMPDIR/archive-demo" --max-bitrate-delta 8 --max-align-ms 1  # -> 0/5
```

或自行 `go build -o archive ./cmd/archive` 后直接运行二进制。

## 核心设计

- **统一时基**：全内核只使用微秒整数 `MediaTime`，禁止各轨独立累计；
  负起点/非零起点作为素材真实属性保留，回绕时间戳在适配器层先展开。
- **取整留痕**：剪辑点落在帧/采样中间时按 nearest/floor/ceil 吸附，
  误差（snap−requested）随片段记录；转场取两轨并集且只计时一次。
- **静音**：按采样窗 RMS/dBFS 判定，跨解码块自动合并，声道缺帧标记
  `Complete=false`。
- **指纹**：`ContentKey` 仅取决于解码内容（同内容不同封装可归并），
  `FullKey` 额外纳入时间跨度/轨道/段类型；转场段独立键空间防误并，
  来源冲突保留全部候选。
- **幂等与提交**：幂等键由输入摘要+排序参数+适配器版本派生；片段文件与
  日志先落盘，检查点才原子提交；中断遗留的 pending 片段与孤儿文件在续跑
  时分别被复用与清理。
- **故障隔离**：单素材损坏/不兼容/非法剪辑点只隔离该素材，不污染其余
  已提交片段；损坏尾包保留已验证前缀并标注首个坏时间。

## 安全与资源约束

- 仅访问用户通过 `--dir` 指定的本地路径；`SafePath` 拒绝绝对路径与
  `..` 穿越。
- 限制单素材大小（默认 512MiB）、轨道数（16）、解码时长（2 分钟）、
  并发数（2），见 `adapter.ParseOptions`。
- 所有写文件采用“临时文件 + fsync + 原子 rename”。
- 构建产物不纳入交付（见 `.gitignore`）。

## 测试

三个核心子系统及全部包均有独立单元测试，`go test ./...` 全绿；
`internal/pipeline` 包含端到端测试，覆盖跨解析、切分、清单的故障传播
与中断续跑、重复运行哈希一致。
