.PHONY: all build test tidy vet acceptance clean

all: build

tidy:
	go mod download
	go mod tidy

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

acceptance:
	bash scripts/acceptance.sh

clean:
	rm -rf bin .relay-ok .relay-bad .relay-recover
