.PHONY: build test run clean

build:
	go build -o bin/kvserver ./cmd/kvserver
	go build -o bin/kvdump ./cmd/kvdump

test:
	go test -race ./...

run: build
	./bin/kvserver -addr :9090 -log-file data/kv.log

clean:
	rm -rf bin/
