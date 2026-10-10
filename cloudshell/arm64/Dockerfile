# Copyright 2021 The KubeCube Authors. All rights reserved.
# Use of this source code is governed by a Apache license
# that can be found in the LICENSE file.

# The base image is built from ./base/Dockerfile and published by
# `make base-image`, which is the only producer of this tag; keep the default
# here and in the Makefile in step.
ARG BASE_IMG=hub.c.163.com/kubecube/ubuntu:16.04-tools
FROM ${BASE_IMG}

# Both architectures carry the same kubectl, which the shell script copies into
# the chroot it builds. Bump KUBECTL_VERSION and KUBECTL_SHA256 together; the
# hash is published next to the binary as <binary-url>.sha256.
ARG KUBECTL_VERSION=v1.22.2
ARG KUBECTL_SHA256=c5bcc7e5321d34ac42c4635ad4f6fe8bd4698e9c879dc3367be542a0b301297b
ARG KUBECTL_BASE_URL=https://dl.k8s.io/release

COPY ./arm64/kubecube-chroot.sh /kubecube-chroot.sh
RUN chmod +x /kubecube-chroot.sh

RUN wget -q -O /bin/kubectl "${KUBECTL_BASE_URL}/${KUBECTL_VERSION}/bin/linux/arm64/kubectl" \
    && echo "${KUBECTL_SHA256}  /bin/kubectl" | sha256sum -c - \
    && chmod +x /bin/kubectl
