BIN := bin/localfront

.PHONY: all build test lint clean

all: build

build:
	go build -o $(BIN) ./cmd/localfront

test:
	go test ./...

lint:
	golangci-lint run

clean:
	rm -rf bin
