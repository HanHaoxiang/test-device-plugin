IMAGE_VERSION = latest
REGISTRY = docker.io/joyme
IMAGE = ${REGISTRY}/test-device-plugin:${IMAGE_VERSION}

.PHONY: build deploy

build:
	CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o build/test cmd/server/app.go

buildImage:
	docker build -t ${IMAGE} .

kindLoad:
	kind load docker-image ${IMAGE}

pushImage:
	docker push ${IMAGE}

deploy:
	helm install test deploy/helm/test

upgrade:
	helm upgrade test deploy/helm/test

dry-run:
	helm install test deploy/helm/test --dry-run