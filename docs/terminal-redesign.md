# webconsole terminal redesign

Status: proposed.

Scope: move the terminal onto the platform's unified Kubernetes access path. `cube` owns cluster
credentials, authorization, impersonation and protocol upgrade; `webconsole` becomes a terminal relay
that reaches the platform over HTTP only.

Paths in this document that start with `KubeCube/` or `kubecube-front/` refer to the sibling
repositories of this workspace. Paths without a prefix are relative to this repository.

## 0. Design inputs

| Fact | Where |
|---|---|
| `cube` already runs an upgrade-aware proxy. `authproxy.Handler` performs JWT authentication, `belongs` authorization, fieldManager annotation and user impersonation, then proxies through `UpgradeAwareHandler` with an `UpgradeTransport`. | `KubeCube/pkg/warden/server/authproxy/authproxy.go:119-146`, `:97-98` |
| That proxy already serves the pivot cluster at `/api/v1/cube/kubernetes/*path`, registered outside the middleware chain. | `KubeCube/pkg/apiserver/cubeapi/k8s/proxy.go:38-53`, `KubeCube/pkg/apiserver/apiserver.go:165` |
| The multi-cluster proxy is a plain `httputil.ReverseProxy` and does not carry an upgrade. | `KubeCube/pkg/apiserver/cubeapi/resourcemanage/handle/proxy.go:216` |
| `multicluster` holds a `Client` and a `*rest.Config` per cluster, which is what the proxy above needs to be constructed. | `KubeCube/pkg/multicluster/manager.go:112,232` |
| Version conversion (`pkg/conversion`) is enabled by default through `EnableVersionConversion`. | `KubeCube/pkg/apiserver/config.go:31-32`, `KubeCube/pkg/conversion/conversion.go:172` |
| `warden` runs the same upgrade-aware proxy in every member cluster. | `KubeCube/pkg/warden/server/apiserver.go:59` |
| `webconsole` is deployed in the pivot cluster only. Member-cluster installs set `componentsEnable.webconsole: "false"`. | `kubecube-chart/README.md:76-82` |
| `webconsole` imports `github.com/kubecube-io/kubecube` at v1.2.0, whose `pkg/multicluster/client` registers the HNC API group in its scheme. The current `cube` registers only `clientgoscheme`, `apis` and `apiextensionsv1`. | `go.mod:9`, `vendor/modules.txt:64`, `vendor/github.com/kubecube-io/kubecube/pkg/multicluster/client/client.go:35,50`, `KubeCube/pkg/multicluster/client/client.go:47-54` |

## 1. Goals and invariants

Goals:

- **G1** The terminal is served by the platform's unified Kubernetes access path.
- **G2** `webconsole` imports no Kubernetes client. It reaches the platform over HTTP, holds no cluster
  credentials and no informer cache.

Invariants that hold throughout:

- The external contract is unchanged: `GET /api/v1/{cluster}/namespace/{ns}/pod/{pod}/shell/{container}`
  returns `{id}`, and `GET /api/sockjs?<id>` carries `{Op: bind|stdin|stdout|resize}` plus `ping`/`pong`.
- The front end needs no change other than a terminal library upgrade.
- The nginx routes in the chart are unchanged.
- Terminal auditing keeps flowing from `webconsole` to the audit service.
- `PodAuthorityVerify` remains the entry check for "may this user enter this pod".

## 2. Target architecture

```
browser (xterm.js)
   |  sockjs  {Op: bind|stdin|stdout|resize} + ping/pong          <- contract unchanged
   v
webconsole (pivot cluster: terminal relay, no Kubernetes client)
   |  1. entry check: may this user enter this pod
   |  2. cluster and pod lookups over HTTP
   |  3. upgrade request to cube, then copy both directions after Hijack
   v
cube  /api/v1/cube/kubernetes/clusters/:cluster/*path
   |  authproxy.Handler: authenticate -> belongs authorize -> fieldManager -> Impersonate-User
   |  UpgradeAwareHandler + UpgradeTransport
   v
target cluster apiserver (pods/exec)
```

| Component | Responsibility |
|---|---|
| webconsole | session registry, sockjs bridging, terminal auditing, entry authorization |
| cube | cluster credentials, upgrade-aware proxying, authorization, impersonation, version conversion on REST paths |
| warden | the same proxy on the member-cluster side, for deployments that reach member clusters through it |

## 3. Batches

| Batch | Content | Depends on |
|---|---|---|
| 1 | Images and base: multi-arch webconsole, cloudshell base image upgrade, chroot script rewrite | — |
| 2 | Terminal front end: xterm upgrade, dead style removal | — |
| 3 | Session lifecycle: expiry and revocation, leader election removal, input parsing fix | — |
| 4 | Unified Kubernetes channel: upgrade-aware multi-cluster proxy in cube, relay in webconsole, cluster lookups over HTTP | §11 item 1 |
| 5 | Terminal capabilities: node shell, debug containers, auditing of interactive sessions | batch 4 |

## 4. Batch 1 — images and base

### 4.1 Multi-arch webconsole image

```dockerfile
# Dockerfile
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} GO111MODULE=on \
    go build -mod=vendor -a -o webconsole main.go
```

```make
# Makefile
docker-build-webconsole-multi-arch:
	docker buildx build -f ./Dockerfile -t $(IMG) \
	  --platform=linux/arm64,linux/amd64 . --push
```

`--platform` lists `linux/arm64` and `linux/amd64` only. `IMG` defaults to the registry that actually
publishes the image.

### 4.2 cloudshell base image: `ubuntu:16.04` to `ubuntu:24.04`

```dockerfile
# cloudshell/base/Dockerfile
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates bash coreutils \
        wget curl vim less tar bzip2 rsync \
        procps net-tools iproute2 iputils-ping \
        lsof tcpdump \
    && rm -rf /var/lib/apt/lists/*

CMD ["sleep", "infinity"]
```

Package decisions. Package availability on noble is not verified in this checkout; confirm each name
at build time.

| Package | Decision | Reason |
|---|---|---|
| `openssh-server`, `supervisor`, `base/sshd.conf`, `EXPOSE 22` | drop | No SSH or port 22 reference exists in this repository or the chart; the cloudshell Deployment declares no `ports` |
| `telnet`, `cron`, `iptables`, `sysstat` | drop | Cleartext protocol; privilege surface; needs `NET_ADMIN`; not used by the chroot script |
| `inetutils-ping` | replace with `iputils-ping` | pick whichever resolves on noble |
| `ca-certificates` | add | The chroot script fetches `https://kubecube:7443` |

### 4.3 Publishing both architectures

**Landed**: `BASE_IMG` and the `--build-arg` wiring below. The default is still
`hub.c.163.com/kubecube/ubuntu:16.04-tools` until §4.2 lands, `IMG` is passed in rather than defaulted, and
`multi-image` does not pass `--amend` yet. The block is the target state.

```make
# cloudshell/Makefile — the base image and the final image need separate variables
BASE_IMG ?= hub.c.163.com/kubecube/ubuntu:24.04-tools
IMG      ?= hub.c.163.com/kubecube/cloudshell:v1.3.0

base-image:
	docker buildx build -f ./base/Dockerfile -t $(BASE_IMG) \
	  --platform=linux/amd64,linux/arm64 . --push

amd64-image:
	docker buildx build -f ./amd64/Dockerfile --build-arg BASE_IMG=$(BASE_IMG) \
	  -t $(IMG)-amd64 --platform=linux/amd64 . --push

arm64-image:
	docker buildx build -f ./arm64/Dockerfile --build-arg BASE_IMG=$(BASE_IMG) \
	  -t $(IMG)-arm64 --platform=linux/arm64 . --push

multi-image: amd64-image arm64-image
	docker manifest create --amend $(IMG) $(IMG)-amd64 $(IMG)-arm64
	docker manifest push $(IMG)
```

The base image must itself be a multi-arch manifest; otherwise the arm64 build fails at `FROM`. It is a
build-time dependency and does not belong in `kubecube-chart/images.list`.

The cloudshell image is published under a **new tag**. Reusing a tag would leave the running pods on the
old image, because the Deployment sets `imagePullPolicy: IfNotPresent`; §9 covers the operator side.

### 4.4 chroot script rewrite

Three rules: architecture independent (directories derived from `ldd`), usrmerge independent (no
assumption about the shape of `/lib`), and failures must be explicit (`set -eo pipefail`).

The script exists twice, at `cloudshell/amd64/kubecube-chroot.sh` and `cloudshell/arm64/kubecube-chroot.sh`.
Replace the two copies with one source generated at build time, or update both in the same change; a
divergence between them is silent.

```bash
#!/bin/bash
set -eo pipefail

declare -a headers; i=0
while getopts "u:c:h:" opt; do
  case $opt in
    u) USERNAME=$OPTARG ;;
    c) CLUSTER=$OPTARG ;;
    h) headers[$i]="--header=$OPTARG"; let i+=1 ;;
    \?) echo "invalid option: -$OPTARG" >&2; exit 2 ;;
  esac
done

: "${USERNAME:?must pass -u <user>}"      # without a user the jail is shared between users
: "${CLUSTER:?must pass -c <cluster>}"
DIR="$HOME/$USERNAME-$CLUSTER"

copy_with_libs() {                        # $1 = source binary, $2 = destination directory
    cp "$1" "$2"
    local lib
    for lib in $(ldd "$1" | grep -oE '/[^ ()]*\.so(\.[0-9]+)*' | grep -v 'linux-vdso' || true); do
        mkdir -p "$DIR$(dirname "$lib")"  # create the parent from the real path
        cp -L "$lib" "$DIR$lib"           # -L resolves symlinks
    done
}

if [ ! -d "$DIR" ]; then
    mkdir -p "$DIR"/{bin,usr/bin,etc,var,tmp,root/.kube,lib,lib64}
    for b in bash ls rm grep cat less mkdir echo; do copy_with_libs "/bin/$b"  "$DIR/bin/";     done
    for b in vi vim xargs;                        do copy_with_libs "/usr/bin/$b" "$DIR/usr/bin/"; done
    cp /bin/kubectl "$DIR/bin/"
    cp /etc/hosts /etc/resolv.conf "$DIR/etc/" 2>/dev/null || true
fi
# credential retrieval and chroot stay as they are
```

- `mkdir -p "$DIR$(dirname "$lib")"` covers arm64 (`aarch64-linux-gnu`) and usrmerge (`/lib` to
  `usr/lib`) at once.
- `-u` is required and validated. Without it the jail path is shared between users and the user-creation
  branch is skipped.
- `chown` takes an explicit list of the directories the user has to write.
- The dynamic loader (`/lib64/ld-linux-x86-64.so.2` on amd64, `/lib/ld-linux-aarch64.so.1` on arm64)
  comes from the `ldd` output; no special case is needed.
- The jail is built only when `$DIR` does not exist. A jail left behind by an older image survives an
  image upgrade; §9 covers its removal.

### 4.5 Verification

```bash
docker run --rm --platform linux/arm64 <base> bash -c 'ldd /bin/bash'
docker run --rm --platform linux/amd64 <base> bash -c 'ldd /bin/bash'

docker run --rm -it <cloudshell-arm64> bash -x /kubecube-chroot.sh -u testuser -c test
# expect no "cannot create regular file"; after chroot, kubectl version --client runs

docker manifest inspect $(BASE_IMG)
```

## 5. Batch 2 — terminal front end

- `xterm@^3.2.0` becomes `@xterm/xterm@^5` with `@xterm/addon-fit`, `@xterm/addon-search` and
  `@xterm/addon-web-links`.
- `Terminal.applyAddon(...)` becomes `term.loadAddon(new FitAddon())`.
- Files: `kubecube-front/src/kubecube/component/global/xterm/utils.js`,
  `kubecube-front/src/kubecube/component/global/xterm/kube-terminal.vue`,
  `kubecube-front/src/kubecube/elComponent/termModal/utils.js`,
  `kubecube-front/src/kubecube/elComponent/qz-terminal/src/qz-terminal.vue`,
  `kubecube-front/src/client/components/common/x-term.vue/index.js`.
- Remove the `.transferWrap`, `.transfer` and `.blank` rules in `qz-terminal.vue`; no element uses them.
- Do not modernize other dependencies; `node-sass` 4 is pinned.

The message contract does not change, so the backend is untouched by this batch.

Verification: `npm run eslint`; `npm run dev` against a live KubeCube for one successful session and one
failure path; `npm run build` when a dependency changes.

## 6. Batch 3 — session lifecycle

1. Give sessions an end. Replace the `connMap sync.Map` with a structure carrying `expiresAt` plus a
   periodic sweep, and delete the entry in the `defer` of `handleTerminalSession`.
2. Remove leader election. `leader` is read only by the `/leader` probe and nothing else consumes it.
   Delete the election and the route, and serve readiness from a `/readyz` that always returns 200. This
   is what allows more than one replica to serve sessions and keeps established sessions alive across a
   rolling update.
3. Fix input parsing. `GetTokenFromReq` splits on `string(bearerToken[6])`, which panics when the
   Authorization header is shorter than seven characters. Split on the space and check the length first.

Verification: sessions survive a rolling restart; a short Authorization header is rejected instead of
panicking; an expired session id cannot be reused.

## 7. Batch 4 — unified Kubernetes channel

### 7.1 Upgrade-aware multi-cluster proxy in cube

```go
// KubeCube/pkg/apiserver/cubeapi/k8s/proxy.go
func (h *Handler) handlerFor(clusterName string) (*authproxy.Handler, error) {
    internal, err := multicluster.Interface().Get(clusterName)
    if err != nil { return nil, err }
    ah := &authproxy.Handler{}
    ah.SetHandlerClient(internal.Client)
    if err := ah.SetHandlerTS(internal.Config); err != nil { return nil, err }
    return ah, nil
}

func (h *Handler) MultiClusterProxy(c *gin.Context) {
    ah, err := h.handlerFor(c.Param("cluster"))
    if err != nil { /* 4xx */ return }
    c.Request.URL.Path = c.Param("path")
    ah.ServeHTTP(c.Writer, c.Request)
}
```

```go
// KubeCube/pkg/apiserver/apiserver.go — registered outside the middleware chain
root.Any(constants.ApiK8sProxyPath+"/clusters/:cluster/*path", k8s.NewHandler().MultiClusterProxy)
```

Cache the handler per cluster name so the transport is not rebuilt per request. The target comes from the
`*rest.Config` that `multicluster` already holds for the cluster, so `cube` dials the member cluster's
apiserver directly.

### 7.2 Middleware constraints

The chain is `precheck -> auth -> audit -> recovery` (`KubeCube/pkg/apiserver/middlewares/middlewares.go:33-39`).
Effect on an upgrade:

| Middleware | Effect | Mechanism |
|---|---|---|
| `precheck.PreCheck()` | breaks the upgrade conditionally | `fetchCluster` falls through to `c.PostForm("cluster")` when cluster is not a path parameter, and `PostForm` parses the request body (`KubeCube/pkg/apiserver/middlewares/precheck/precheck.go:57`) |
| `auth.Auth()` | none | does not wrap the ResponseWriter and does not read the body |
| `audit.Audit()` | breaks the upgrade | for non-GET requests it replaces `c.Writer` with a wrapper that does not implement `Hijacker` (`KubeCube/pkg/apiserver/middlewares/audit/audit.go:88`, `:398-411`), and it reads the body for POST proxy APIs (`:413-421`) |
| `recovery.Recovery()` | none | plain `gin.Recovery()` (`KubeCube/pkg/apiserver/middlewares/recovery/recovery.go:24`) |

**Rule R1.** Register the exec route in `apisOutsideMiddlewares`, as the pivot route
`/api/v1/cube/kubernetes/*path` already is. This covers both cases above and does not go stale when a
middleware is added later.

**Constraint R2.** The route carries `:cluster` as a path parameter. Cluster name never arrives through
the query string or the body.

**Constraint R3.** Reuse `authproxy.Handler`, not `ProxyHandler`. The former has no version conversion,
so subresources such as `pods/exec` are never rewritten.

### 7.3 Relay in webconsole

Removed:

- `getNonControlCfg`, `initKubeConf` and the `configMap` kubeconfig cache
  ([handler/apihandler.go:172-215](../handler/apihandler.go#L172-L215))
- `remotecommand`, `NewSPDYExecutor` and `StreamOptions`
  ([handler/terminal.go:303-319](../handler/terminal.go#L303-L319))
- `isNsOrPodBelongToNamespace`, whose job `belongs` does in cube
  ([handler/auth.go:119-149](../handler/auth.go#L119-L149))
- `GetClusterInfoByName` and `GetPivotCluster` in their current form
  ([handler/cluster.go:31-67](../handler/cluster.go#L31-L67)); both read `Cluster` resources through
  `clients.Interface().Kubernetes(constants.LocalCluster)`
- the `github.com/kubecube-io/kubecube` imports of `pkg/clients`, `pkg/apis/cluster/v1` and
  `pkg/utils/kubeconfig`; `pkg/clog` and `pkg/utils/constants` remain

Added:

- an upgrade request to `https://<cube>/api/v1/cube/kubernetes/clusters/{cluster}/{k8s_url}`
- a bidirectional copy between the hijacked connection and the sockjs session
- cluster and pod lookups through cube's HTTP APIs, replacing the direct reads above; the CloudShell flow
  resolves its target pod the same way
- `PodAuthorityVerify` stays as the entry check

G2 falls out of the removed imports: `go mod vendor` stops vendoring `pkg/clients` and
`pkg/multicluster`, and with them `sigs.k8s.io/hierarchical-namespaces`, which only
`pkg/multicluster/client` pulls in.

### 7.4 Authorization chain

webconsole entry check, then `belongs.RelationshipDetermine` in cube, then `Impersonate-User`, then the
target apiserver decides by that user's RBAC.

### 7.5 Verification

| Assertion | How |
|---|---|
| Structure | A test asserts the exec route is not registered under the group carrying middlewares, or that its handler chain holds no middleware |
| Behaviour | Send `Connection: Upgrade` to the route and assert `101 Switching Protocols`, or assert the `ResponseWriter` the handler receives satisfies `http.Hijacker` |
| Regression | The behaviour assertion must fail if the route is later moved into the middleware chain |
| Conversion | Assert the exec URL is not rewritten and `tryVersionConvert` is not reached |
| Semantics | On amd64 and arm64, a session from the UI behaves like `kubectl exec` |
| Authorization | A user of tenant A cannot exec into a pod of tenant B, and the refusal comes from apiserver RBAC |
| Version conversion | On a cluster that serves only non-preferred versions, REST resource paths still work |
| RBAC | On clusters at or above 1.35, every role holds `create` on `pods/exec`, required by the synthetic CREATE check on websocket upgrades |
| No Kubernetes client (G2) | `go list -deps ./... \| grep -E 'kubecube-io/kubecube/pkg/(clients\|multicluster)'` returns nothing |
| Vendor tree | `grep -r hierarchical-namespaces vendor/` returns nothing, and `vendor/modules.txt` lists neither `kubecube/pkg/multicluster` nor `sigs.k8s.io/hierarchical-namespaces` |
| Credentials | No cluster kubeconfig exists inside the webconsole process |

## 8. Batch 5 — terminal capabilities

### 8.1 Node shell

- New route `GET /api/v1/{cluster}/node/{node}/shell` returning `{id}`, reusing the sockjs contract.
- On session bind, create a privileged pod through cube's REST proxy: `nodeName`, `hostPID: true`,
  `hostPath: /` mounted at `/host`, `privileged: true`, an image configurable through
  `--nodeShellImage`, entering with `nsenter -t 1 -m -u -i -n -p -- bash`.
- Lifecycle: label `kubecube.io/node-shell=<session>`, delete on session close, and sweep orphans with
  the expiry mechanism from batch 3.
- Authorization: the node-level check in cube (resource `nodes`) is mandatory, and the session is audited.

### 8.2 Debug containers

- New route `POST /api/v1/{cluster}/namespace/{ns}/pod/{pod}/debug` with image and target container in
  the body. It calls `pods/ephemeralcontainers` through cube's REST proxy, returns the container name,
  and the existing exec route is used from there.
- Feature gate: ephemeral containers are GA in Kubernetes 1.25. Below that, hide the entry and return an
  explicit error.
- Permissions: `get` and `update` on `pods/ephemeralcontainers`.

### 8.3 Auditing interactive sessions

- Record stdin as raw byte blocks rather than only on carriage return, so interactive programs such as
  `vim`, `top` and `kubectl edit` leave a trace.
- Add `session_start`, `session_end` and `resize` event types to `AuditMsg`.
- Make the recording level configurable: `none`, `stdin`, `stdout`, `all`.
- Session recording and playback are a separate follow-up.

## 9. Upgrade

This section is the source for the `## Upgrade` entry in `kubecube-chart/changelog.md` and for the
runbook section in `kubecube-chart/README.md`. Helm deletes only what it still renders, so every item
below is state that survives an upgrade on its own.

| Surviving state | Why it survives | Action |
|---|---|---|
| The old cloudshell image | the Deployment sets `imagePullPolicy: IfNotPresent`, so a reused tag is never re-pulled | publish the new image under a new tag, point `global.images.cloudshell` at it, and confirm the Deployment rolled |
| Chroot jails under `$HOME` | the script builds a jail only when `$DIR` is absent, so a jail created by the old image stays | delete `$HOME/*-<cluster>` inside the cloudshell pods, or restart the cloudshell Deployment, so the next session rebuilds the jail |
| Long-lived kubeconfig tokens | tokens already issued stay valid until they expire | decide between rotation and letting them expire; state the decision in the changelog |
| Sessions in flight | sessions live in the webconsole process | note that a rolling restart ends them until batch 3 lands |

Verification after the upgrade:

```console
kubectl -n kubecube-system exec deploy/cloud-shell -- ls /root
# expect no <user>-<cluster> directories before the first new session

kubectl -n kubecube-system get deploy cloud-shell \
  -o jsonpath='{.spec.template.spec.containers[0].image}'
# expect the new tag
```

## 10. Reference cleanup

Descriptions that stop being true once batch 4 lands, and the places that carry a second copy of a fact
owned elsewhere:

| Reference | Change |
|---|---|
| `README.md` and `README-zh_CN.md`, "Connection Process" | both state that the server and the container communicate over SPDY. Update both sides in the same commit, as the pair rule requires |
| `AGENTS.md`, repository table | describes webconsole as `webconsole <-> pod over SPDY`. Update the row |
| `deploy/*.yaml` | unused copies of the webconsole and cloudshell manifests; align them or remove them |
| `kubecube-chart/images.list` and `values.yaml` | the cloudshell and webconsole image tags; change together with the chart version |
| `kubecube-chart/Chart.yaml` | `version` for the template change, `appVersion` when the platform version moves |

## 11. Prerequisites and verification matrix

Prerequisites for batch 4:

1. **Reachability from the pivot cluster to member-cluster apiservers.** When it holds, §7.1 applies: the
   handler is built from the `*rest.Config` in `multicluster`. When the pivot can only reach `warden`, add
   an endpoint field to `WardenInfo` and the Cluster CR, and point `handlerFor` at that address instead.
2. **Ownership of terminal auditing.** Terminal streams keep flowing from webconsole to the audit
   service. The `/proxy` auditing in cube covers REST and does not see streamed sessions.

Verification matrix:

| Item | Method |
|---|---|
| Multi-arch images | `docker manifest inspect` for webconsole, cloudshell and the base image |
| chroot correctness | `bash -x` on both architectures with no `cp` failure; `kubectl` runs after chroot |
| Contract unchanged | One session completes with no front-end change |
| Session lifecycle | Sessions survive a rolling restart; an expired id stops working |
| Front end | `npm run eslint`, `npm run dev` including one failure path, `npm run build` when a dependency changes |
| Chart | `helm lint`, `helm template` |
| Cross-repository consistency | `values.yaml`, `images.list`, `Chart.yaml`, `vendor/`, and the changelogs of every repository touched |
| Upgrade | the checks in §9 return the expected values on a cluster upgraded from the previous release |

When a middleware is added, check that it neither wraps the `ResponseWriter` (which would break `Hijack`)
nor parses the request body (which would consume the stream). Grep for `c.Writer =`, `PostForm`,
`FormValue`, `ParseForm`, `ParseMultipartForm` and `Hijack`.

## 12. Not verified in this checkout

- Package availability and the exact `ldd` output on `ubuntu:24.04`, on both architectures.
- Whether `chroot` is usable inside a jail built on a usrmerge system.
- The behaviour of `chroot --userspec=` when the user part is empty.
- Published manifest lists for the images this design depends on.
- Reachability between the pivot cluster and member-cluster apiservers.
- Whether cube's cluster HTTP APIs cover every field the CloudShell flow reads from a `Cluster`
  resource today.
