GO ?= go

.PHONY: all build test selftest verify clean vendor

all: build

build:
	$(GO) build -mod=vendor ./...

test:
	$(GO) test -mod=vendor ./...

selftest:
	$(GO) run -mod=vendor ./cmd/subtool selftest --out ./data --seed 20261115 --cues 2400

verify:
	$(GO) run -mod=vendor ./cmd/subtool verify --in ./data --matrix all --max-drift-ms 1 --json

vendor:
	$(GO) mod tidy && $(GO) mod vendor

clean:
	rm -rf data reports build out dist target
