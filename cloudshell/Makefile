# Copyright 2021 The KubeCube Authors. All rights reserved.
# Use of this source code is governed by a Apache license
# that can be found in the LICENSE file.

# BASE_IMG is built from ./base/Dockerfile and is the FROM of both architecture
# images, so it has to be a multi-arch manifest and has to be pushed before
# them. IMG is the published manifest: pass a new tag rather than reusing a
# published one, because the Deployment pulls with IfNotPresent and would keep
# running the old image.
BASE_IMG ?= hub.c.163.com/kubecube/ubuntu:16.04-tools

base-image:
	docker buildx build -f ./base/Dockerfile -t ${BASE_IMG} --platform=linux/arm64,linux/amd64 . --push

amd64-image:
	docker buildx build -f ./amd64/Dockerfile --build-arg BASE_IMG=${BASE_IMG} -t ${IMG}-amd64 --platform=linux/amd64 . --push

arm64-image:
	docker buildx build -f ./arm64/Dockerfile --build-arg BASE_IMG=${BASE_IMG} -t ${IMG}-arm64 --platform=linux/arm64 . --push

multi-image: amd64-image arm64-image
	docker manifest create ${IMG} ${IMG}-amd64 ${IMG}-arm64
	docker manifest push ${IMG}
