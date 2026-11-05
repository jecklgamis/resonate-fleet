build:
	go build -o resonate-worker .
	go build -o resonate-fleet ./cmd/resonate-fleet

test:
	go test ./...

check: fmt-check vet test

fmt:
	gofmt -w .

fmt-check:
	@diff=$$(gofmt -l .); if [ -n "$$diff" ]; then echo "gofmt needed on:"; echo "$$diff"; exit 1; fi

vet:
	go vet ./...

IMAGE ?= jecklgamis/resonate-worker
TAG ?= main

image:
	docker build -t $(IMAGE):$(TAG) .

run: build
	./resonate-worker

.PHONY: build test check fmt fmt-check vet image run
