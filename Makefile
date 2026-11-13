.PHONY: test demo vet

test:
	go test ./...

vet:
	go vet ./...

demo:
	go run ./cmd/crawler demo --pages 600 --sites 3 --db data/crawl.db --report report.json
