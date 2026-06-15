BIN := bin/localfront

.PHONY: all build test e2e lint clean

all: build

build:
	go build -o $(BIN) ./cmd/localfront

test:
	go test ./...

# Integration tests. Needs Docker (S3 scenarios) and the runn CLI for the
# examples/ scenarios (install with `aqua i`); both skip when unavailable.
e2e:
	go test -tags e2e ./e2e/...

lint:
	golangci-lint run

clean:
	rm -rf bin
