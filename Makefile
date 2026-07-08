VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v1.0.0)
IMAGE  := ghcr.io/leonlau/alertmanager-webhook-feishu

fmt:
	go fmt ./...
run:fmt
	go run main.go server -c config.yml -v
build:
	goreleaser release --snapshot
docker_build:
	docker build -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .
docker_push:docker_build
	docker push $(IMAGE):$(VERSION)
	docker push $(IMAGE):latest
