BIN := bin/segctl
export GOTOOLCHAIN := local

.PHONY: build test demo clean

build:
	go build -o $(BIN) ./cmd/segctl

test:
	go test ./...

demo: build
	sh scripts/demo.sh

clean:
	rm -rf bin testdata out
