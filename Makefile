.PHONY: build test run clean

build:
	go build -o bin/kvserver ./cmd/kvserver

test:
	go test -race ./...

run: build
	./bin/kvserver -addr :9090 -log-file data/kv.log

clean:
	rm -rf bin/
