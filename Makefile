GO ?= go
BIN ?= bin/mediacore

.PHONY: tidy build test vet demo verify-fault clean

tidy:
	$(GO) mod tidy

build:
	$(GO) build ./...

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# Both acceptance commands with metric assertions.
demo:
	./scripts/demo.sh $(BIN)

# Corrupt-sample run: exit code 2 is the expected, successful outcome.
verify-fault:
	$(GO) build -o $(BIN) ./cmd/mediacore
	-$(BIN) verify --self-generate --fault truncate --simulate-crash-after 5 --resume

clean:
	rm -rf bin
