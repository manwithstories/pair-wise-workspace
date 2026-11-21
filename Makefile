# mediacheck — 区域广播多码率母版批量检查与再封装（纯本地）
#
# 典型流程（macOS，Go 1.23+；本机已装 Go 可跳过 brew 步骤）:
#   brew install go
#   make tidy build test demo
#
# 演示命令等价于验收命令，所有样本由测试/演示入口自生成，无外部素材与网络依赖。

.PHONY: tidy build test test-race demo clean

# 固定验收参数；可用 make demo DURATION=60s 缩短运行时间。
SEED            ?= 42
DURATION        ?= 7200s
REPS            ?= 360p,720p,1080p
SEGMENT         ?= 6s
CHECKPOINT_EVERY ?= 256
LOSS_BURST      ?= 12
TRUNCATE_TAIL   ?= 1
WORK_DIR        ?= ./.demo-work

tidy:
	go mod tidy

build:
	go build ./...

test:
	go test ./...

test-race:
	go test -race ./...

demo: build
	go run ./cmd/mediacheck demo \
		--seed $(SEED) \
		--duration $(DURATION) \
		--representations $(REPS) \
		--segment $(SEGMENT) \
		--checkpoint-every $(CHECKPOINT_EVERY) \
		--loss-burst $(LOSS_BURST) \
		--truncate-tail $(TRUNCATE_TAIL) \
		--work-dir $(WORK_DIR)

clean:
	rm -rf ./.demo-work
