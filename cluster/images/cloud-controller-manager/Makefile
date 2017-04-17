
REGISTRY?=registry.cn-hangzhou.aliyuncs.com/google-containers
IMAGE?=cloud-controller-manager
ARCH?=amd64
NUM?=1
VERSION?=1.6.1
REVISION:=$(shell git rev-parse --short HEAD)

cp:
	rm -rf cloud-controller-manager
	cp ../../../_output/dockerized/bin/linux/amd64/cloud-controller-manager .

build: cp
	echo ${VERSION}
	docker build -t ${REGISTRY}/${IMAGE}-${ARCH}:${VERSION}-${REVISION}.${NUM} .
	rm -rf cloud-controller-manager

push:
	docker push ${REGISTRY}/${IMAGE}-${ARCH}:${VERSION}-${REVISION}.${NUM}

all: build push
	echo "begain"