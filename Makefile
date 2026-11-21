.PHONY: build test race accept tidy clean

BINARY := avcheck

build:
	go build -o $(BINARY) ./cmd/avcheck

test:
	go test ./...

race:
	go test -race ./...

tidy:
	go mod tidy

accept: build
	./$(BINARY) -duration 10s -tracks 3 -seed 7 -loss-burst 3 -reorder 8 \
		-duplicate 5 -queue-bytes 262144 -out /tmp/avcheck.mp4
	./$(BINARY) -fixture corrupt-wav; true
	./$(BINARY) -fixture queue-boundary

clean:
	rm -f $(BINARY)
