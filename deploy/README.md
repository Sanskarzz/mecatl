# Deploying ozzd

Kubernetes manifests for the ozzharness server, `ozzd`. The container image is
built with [`ko`](https://ko.build) directly from `./cmd/ozzd` — there is no
Dockerfile. Manifests reference the image via the `ko://…` placeholder, which
`ko resolve` / `ko apply` substitutes with the real ref at deploy time.

## ⚠️ Security: the ozzd API is UNAUTHENTICATED

ozzd exposes command and file execution over gRPC + HTTP with **no caller
identity check** (see `cmd/ozzd/main.go`). The in-code default binds loopback
for exactly this reason. The Deployment overrides that to `0.0.0.0` because a
pod's network namespace is isolated — but that only shifts the trust boundary to
Kubernetes networking.

- Deploy **only inside a trusted network**, or **behind an external auth proxy /
  mTLS gateway**. Authentication is future work.
- The `Service` is `ClusterIP` only. Do **not** expose it via
  LoadBalancer/NodePort/Ingress without an auth boundary in front.
- Recommended: add a `NetworkPolicy` restricting ingress to ozzd to only the
  clients that need it.

## Prerequisites

- `ko` installed — https://ko.build/install/
- A container registry, set via `KO_DOCKER_REPO`
  (e.g. `export KO_DOCKER_REPO=ghcr.io/stacklok/ozzharness`).
- `kubectl` with access to the target cluster.
- The `ozzd-openai` Secret containing `OPENAI_API_KEY` (see below).

## The OpenAI API key Secret

The Deployment passes `--openai` and reads `OPENAI_API_KEY` from a Secret named
`ozzd-openai`. Create it out-of-band — never commit a real key:

```sh
kubectl create secret generic ozzd-openai \
  --from-literal=OPENAI_API_KEY="sk-...your-key..."
```

`secret.example.yaml` is an illustrative placeholder (plus a commented
`ExternalSecret` template for the External Secrets Operator). It is **not** in
`kustomization.yaml`, so a `kubectl apply -k deploy/` will not push a dummy key.

## Build a local image

```sh
task ko:build          # KO_DOCKER_REPO=ko.local, --local, no push
```

This loads the image into your local Docker/podman daemon and prints the
fully-qualified ref (content-hash tag).

## Build, push, and deploy to a cluster

```sh
export KO_DOCKER_REPO=ghcr.io/stacklok/ozzharness   # your registry

task ko:publish                          # build + push the image
task ko:resolve | kubectl apply -f -      # render ko://… → real ref, then apply
```

`task ko:resolve` runs `ko resolve -f deploy/`, which builds+pushes the image and
emits the manifests with the `ko://…` placeholder replaced. Pipe straight into
`kubectl apply -f -`.

> Note: `ko resolve -f deploy/` processes the YAML files; `kustomization.yaml`
> is for `kubectl apply -k deploy/` (which does not do ko substitution). Use the
> `ko resolve | kubectl apply -f -` flow for the ko-built image.

## Probes: TCP-socket today, /healthz is a follow-up

ozzd has **no HTTP health endpoint** yet. The readiness/liveness probes are
therefore **TCP-socket** probes against the gRPC port — they only confirm the
listener accepts connections, not that the app is healthy. Once a real
`/healthz` (and `/readyz`) lands, swap these to `httpGet` probes in
`deployment.yaml`.

## Pod Security Standards: restricted

Both the pod- and container-level `securityContext` satisfy the PSS
**restricted** profile: `runAsNonRoot` (UID/GID 65532, matching the chainguard
static base), `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`,
`seccompProfile: RuntimeDefault`, and `capabilities.drop: [ALL]`.
