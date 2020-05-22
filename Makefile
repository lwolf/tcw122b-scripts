VERSION ?= $(shell git rev-list --count HEAD)-$(shell git rev-parse --short=7 HEAD)
# Image URL to use all building/pushing image targets
IMG ?= lwolf/tcw122b:$(VERSION)

.PHONY: build
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X main.Version=${VERSION}" -o bin/lights main.go

.PHONY: test
test:
	go test .

# Build the docker image
docker-build: build test
	docker build . -t ${IMG}

# Push the docker image
docker-push:
	docker push ${IMG}

.PHONY: deploy
deploy:
	VERSION=${VERSION} envsubst < deploy/deployment.yaml | kubectl apply -f -