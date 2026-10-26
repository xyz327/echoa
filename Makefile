IMAGE ?= echoa
TAG ?= latest
DOCKERFILE ?= Dockerfile

.PHONY: build test image compose-up compose-down

build:
	go build -buildvcs=false -o bin/server ./cmd/server

test:
	go test ./...

image:
	docker build -f $(DOCKERFILE) -t $(IMAGE):$(TAG) .

compose-up:
	docker compose up -d

compose-down:
	docker compose down
