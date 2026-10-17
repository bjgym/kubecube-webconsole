# Copyright 2021 The KubeCube Authors. All rights reserved.
# Use of this source code is governed by a Apache license
# that can be found in the LICENSE file.

# A supported Ubuntu, not the 16.04 this used to be. 16.04 is past end of life,
# its archive is no longer on the mirrors, and a base image that cannot be
# rebuilt is a base image nobody can patch.
#
# The packages are the ones the session needs:
#   ca-certificates, wget, curl   fetch the user's kubeconfig over https, and the
#                                 architecture images download kubectl over https;
#   openssh-server, supervisor    the session itself: sshd runs under supervisord;
#   bash, findutils               copied into the chroot (bash, ls, rm, grep, cat,
#                                 less, mkdir, echo, xargs);
#   vim                           vi and vim, copied into the chroot;
#   tar, bzip2, rsync, less, lsof, net-tools, inetutils-ping, telnet, tcpdump,
#   iptables, sysstat, cron       what an operator expects in a terminal.
#
# Build it with `make base-image`, which passes ./cloudshell as the context so the
# COPY below resolves; cloudshell/{amd64,arm64}/Dockerfile then take the tag as
# BASE_IMG.
FROM ubuntu:24.04

LABEL org.opencontainers.image.authors="KubeCube Authors" \
      org.opencontainers.image.source="https://github.com/kubecube-io/kubecube-webconsole"

# sshd wants this directory to exist before it starts.
RUN mkdir -p /var/run/sshd \
    && apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates openssh-server vim tar wget curl rsync bzip2 iptables \
        tcpdump less telnet net-tools lsof sysstat cron supervisor inetutils-ping \
    && rm -rf /var/lib/apt/lists/*

# Root login is how a session is reached. The substitution is anchored rather
# than matching one distribution's spelling of the line, which is what the 16.04
# version did: on a newer sshd_config that string is not there at all, so the sed
# matched nothing, succeeded, and left root login disabled — a failure that only
# shows up when someone tries to log in.
RUN sed -i -E 's/^#?[[:space:]]*PermitRootLogin[[:space:]].*/PermitRootLogin yes/' /etc/ssh/sshd_config \
    && grep -q '^PermitRootLogin yes$' /etc/ssh/sshd_config

EXPOSE 22

COPY ./base/sshd.conf /etc/supervisor/conf.d/sshd.conf
CMD ["/usr/bin/supervisord"]
