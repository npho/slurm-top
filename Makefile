GO ?= go
BINARY ?= slurm-top

.PHONY: all build test vet fmt clean

all: build

build:
	$(GO) build -o $(BINARY) .

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

clean:
	rm -f -- $(BINARY)
