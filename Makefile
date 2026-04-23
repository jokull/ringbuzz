.PHONY: build install test test-race tidy clean run

BIN := ringbuzz
PREFIX := $(HOME)/bin

build:
	go build -o $(BIN) ./cmd/ringbuzz

install: build
	install -m 0755 $(BIN) $(PREFIX)/$(BIN)

test:
	go test ./... -count=1

test-race:
	go test -race ./... -count=1

tidy:
	go mod tidy

clean:
	rm -f $(BIN)
	rm -rf dist/

run: build
	./$(BIN) daemon
