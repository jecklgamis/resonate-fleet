build:
	go build -o resonate-worker .
	go build -o resonatectl ./cmd/resonatectl
	go build -o resonate-controller ./cmd/resonate-controller

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
CONTROLLER_IMAGE ?= jecklgamis/resonate-controller
TAG ?= main

image:
	docker build -t $(IMAGE):$(TAG) .

image-controller:
	docker build -f Dockerfile.controller -t $(CONTROLLER_IMAGE):$(TAG) .

run: build
	./resonate-worker

.PHONY: build test check fmt fmt-check vet image image-controller run
