.PHONY: build test run run-cluster stop-cluster clean

PEERS := http://localhost:9091=localhost:7001,http://localhost:9092=localhost:7002,http://localhost:9093=localhost:7003

build:
	go build -o bin/kvserver ./cmd/kvserver
	go build -o bin/kvdump ./cmd/kvdump

test:
	go test -race ./...

# Single-node cluster (one voter, immediately becomes leader).
run: build
	mkdir -p data/node1
	./bin/kvserver \
		-node-id http://localhost:9090 \
		-http-addr :9090 \
		-raft-addr localhost:7000 \
		-data-dir data/node1

# Three-node cluster. Nodes are started in the background; use stop-cluster to stop.
run-cluster: build
	mkdir -p data/node1 data/node2 data/node3
	./bin/kvserver -node-id http://localhost:9091 -http-addr :9091 -raft-addr localhost:7001 -data-dir data/node1 -peers "$(PEERS)" &
	./bin/kvserver -node-id http://localhost:9092 -http-addr :9092 -raft-addr localhost:7002 -data-dir data/node2 -peers "$(PEERS)" &
	./bin/kvserver -node-id http://localhost:9093 -http-addr :9093 -raft-addr localhost:7003 -data-dir data/node3 -peers "$(PEERS)" &
	@echo "cluster started on :9091-9093  |  stop with: make stop-cluster"

stop-cluster:
	pkill -f kvserver || true

clean:
	rm -rf bin/ data/
