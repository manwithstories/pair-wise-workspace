# fieldcut 本地构建与验收入口。
#
# 说明：pion/rtp v1.10.6 的 go.mod 声明 go 1.24.0，而本项目按任务要求固定
# go 1.23 指令。Go 工具链因此在默认只读模式下会要求重写 go.mod；使用
# -mod=mod 即可在保持 1.23 指令与固定依赖版本的前提下正常构建测试。
GOFLAGS ?= -mod=mod

.PHONY: all tidy build test demo demo-truncated demo-late clean

all: build test

tidy:
	go mod tidy
	go mod edit -go=1.23

build:
	go build $(GOFLAGS) ./...

test:
	go test $(GOFLAGS) ./...

vet:
	go vet $(GOFLAGS) ./...

demo:
	go run $(GOFLAGS) ./cmd/fieldcut demo --seed 41 --duration 300s \
	  --queue-bytes 8388608 --late-window 4s --inject-loss 0.02 \
	  --slow-consumer-ms 25

demo-truncated:
	go run $(GOFLAGS) ./cmd/fieldcut demo --case truncated-box --seed 41

demo-late:
	go run $(GOFLAGS) ./cmd/fieldcut demo --case late-outside-window --seed 41

clean:
	rm -rf out/
