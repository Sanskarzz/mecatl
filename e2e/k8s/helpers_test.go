//go:build kind_e2e

// Package k8s_e2e_test is the kind-based Kubernetes cloud-native PROOF for
// mecak8s (ADR 0048, MECAK8S-PLAN §4h). It spins a real kind cluster with a
// Redis StatefulSet + two storage-free agent replicas and asserts three
// cloud-native properties over the HTTP API:
//
//  1. Lease exclusion across replicas (the single-writer proof).
//  2. Graceful failover releases the session lease before the TTL.
//  3. Session persistence across an agent pod restart (Redis-backed).
//
// It is GATED behind the `kind_e2e` build tag so `task test` never compiles
// it; it runs via `task e2e:k8s` (needs Docker + kind + ko + kubectl). The
// suite skips gracefully (ginkgo.Skip, not a failure) when a tool is missing,
// so a bare `go test -tags kind_e2e` without the toolchain does not hard-fail.
//
// Unlike e2e_test (which spawns local mecated processes), this suite drives
// pods via kubectl/port-forward — it imports NO engine/ code and shares none of
// the e2e_test harness.
package k8s_e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// Cluster + manifest constants (ADR 0048 §4h, deploy/mecak8s/).
const (
	kindClusterName = "mecatl-e2e"
	kindNodeImage   = "kindest/node:v1.34.3"
	k8sNamespace    = "mecatl"
	agentComponent  = "agent" // app.kubernetes.io/component label value
	partOfLabel     = "mecak8s"
	agentPodPort    = 8081 // the HTTP/SSE listener (--http-addr default in the pod)
)

// --- tool availability / cluster lifecycle -----------------------------------

// requireTools skips the suite (ginkgo.Skip) if a container runtime (Docker OR
// podman), kind, ko, or kubectl is not on PATH. This is a tool-availability
// gate, not a failure — the Taskfile also has preconditions, but the Go code
// skips too so a bare `go test -tags kind_e2e` does not hard-fail on a machine
// without the toolchain. The suite header says "Needs Docker + kind + ko +
// kubectl", but podman is an ACCEPTABLE ALTERNATIVE container runtime: kind uses
// it via KIND_EXPERIMENTAL_PROVIDER=podman, `kind load docker-image` works
// against podman's Docker-compatible CLI, and ko loads into the local podman
// daemon. So EITHER docker OR podman satisfies the runtime requirement (the
// other three tools are mandatory); without a runtime those commands hit an
// Expect and fail the spec instead of skipping.
func requireTools() {
	ginkgo.GinkgoHelper()
	// Docker OR podman (kind supports podman via KIND_EXPERIMENTAL_PROVIDER=podman).
	_, dockerErr := exec.LookPath("docker")
	_, podmanErr := exec.LookPath("podman")
	if dockerErr != nil && podmanErr != nil {
		ginkgo.Skip("neither docker nor podman is on PATH — skipping the kind k8s e2e suite")
	}
	for _, tool := range []string{"kind", "ko", "kubectl"} {
		if _, err := exec.LookPath(tool); err != nil {
			ginkgo.Skip(fmt.Sprintf("%q is not on PATH — skipping the kind k8s e2e suite (needs container runtime + kind + ko + kubectl)", tool))
		}
	}
}

// kindCreateCluster creates a fresh kind cluster. It is idempotent-ish: if a
// cluster with the same name already exists (a prior run left it), it is
// deleted first so the suite starts from a clean slate. The kind node image is
// pulled by kind itself.
func kindCreateCluster() {
	ginkgo.GinkgoHelper()
	// Best-effort delete of a leftover cluster (ignore errors — it may not exist).
	_ = exec.Command("kind", "delete", "cluster", "--name", kindClusterName).Run()
	runCmd(ginkgoSuiteCtx(), "kind", "create", "cluster",
		"--name", kindClusterName, "--image", kindNodeImage)
}

// kindDeleteCluster tears the cluster down. Called in AfterSuite / DeferCleanup.
func kindDeleteCluster() {
	_ = exec.Command("kind", "delete", "cluster", "--name", kindClusterName).Run()
}

// koBuildMecak8s builds the mecak8s image locally with ko (--bare writes the
// image to the local container daemon and prints the ref) and returns the image
// ref. KO_DOCKER_REPO=ko.local is set so the ref is ko.local:<sha>.
//
// The image is loaded into whichever local daemon (docker or podman) ko detects;
// with KO_DOCKER_REPO=ko.local, `ko build --bare ./cmd/mecak8s` loads into the
// local daemon without needing a registry push.
//
// IMPORTANT: ko build produces `ko.local:<sha>` (a bare tag), but ko resolve
// produces `ko.local/mecak8s-<hash>:<sha>` (a repo/tag). The kind node needs the
// image under the RESOLVED name (what the pod references), so after building we
// resolve the manifests, extract the resolved image ref, and retag the built
// image to match before loading into kind.
func koBuildMecak8s() string {
	ginkgo.GinkgoHelper()
	ctx := ginkgoSuiteCtx()
	cmd := exec.CommandContext(ctx, "ko", "build", "--bare", "./cmd/mecak8s")
	cmd.Dir = repoRoot()
	cmd.Env = append(cmd.Environ(), "KO_DOCKER_REPO=ko.local")
	out, err := cmd.Output()
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred(),
		"ko build ./cmd/mecak8s failed\n--- ko output ---\n%s", out)
	builtRef := strings.TrimSpace(string(out))
	gomega.ExpectWithOffset(1, builtRef).NotTo(gomega.BeEmpty(), "ko build printed no image ref")
	return builtRef
}

// resolvedImageRef renders the manifests with ko resolve and extracts the agent
// container's image ref — the EXACT string the pod will reference. This is
// needed because ko build produces `ko.local:<sha>` but ko resolve produces
// `ko.local/mecak8s-<hash>:<sha>`, and the kind node must have the image under
// the resolved name.
func resolvedImageRef() string {
	ginkgo.GinkgoHelper()
	ctx := ginkgoSuiteCtx()
	resolve := exec.CommandContext(ctx, "ko", "resolve", "-f", "deploy/mecak8s/")
	resolve.Dir = repoRoot()
	resolve.Env = append(resolve.Environ(), "KO_DOCKER_REPO=ko.local")
	out, err := resolve.Output()
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred(),
		"ko resolve failed\n--- output ---\n%s", out)
	// Extract the image: line that starts with ko.local/ (the agent image, not redis).
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "image: ko.local/") {
			return strings.TrimPrefix(line, "image: ")
		}
	}
	ginkgo.Fail("could not find the resolved agent image ref in ko resolve output")
	return ""
}

// retagImage tags a source image as a target ref so kind load can load it under
// the name the pod references. Uses docker OR podman (whichever is on PATH).
func retagImage(src, dst string) {
	ginkgo.GinkgoHelper()
	runtime := "docker"
	if _, err := exec.LookPath("podman"); err != nil {
		// docker is the fallback; podman is preferred when KIND_EXPERIMENTAL_PROVIDER=podman
	} else {
		runtime = "podman"
	}
	runCmd(ginkgoSuiteCtx(), runtime, "tag", src, dst)
}

// kindLoadImage loads a local image into the kind cluster's node so the pod can
// pull it without a registry (kind nodes do not share the host container daemon).
// `kind load docker-image` works against EITHER docker OR podman (the latter via
// KIND_EXPERIMENTAL_PROVIDER=podman, which makes podman's Docker-compatible CLI
// serve the load); no change is needed for the podman path.
func kindLoadImage(image string) {
	ginkgo.GinkgoHelper()
	runCmd(ginkgoSuiteCtx(), "kind", "load", "docker-image", image,
		"--name", kindClusterName)
}

// applyManifests renders the deploy/mecak8s/ kustomize base with ko (substituting
// the ko:// image placeholder with the built ref) and applies it. ko resolve
// reads KO_DOCKER_REPO + the local container daemon; the image MUST already be
// loaded into the kind node (kindLoadImage) so the pod can pull it.
//
// The Kustomization resource itself leaks into ko resolve's output (a ko
// version behavior); it must be filtered out before kubectl apply, or the API
// server rejects it as an unknown kind.
//
// The namespace is applied FIRST and waited on before the rest: kubectl apply
// processes documents in order, but the namespace's "active" state is set by
// the namespace controller asynchronously — resources in that namespace fail
// with "namespaces not found" if applied in the same invocation.
func applyManifests() {
	ginkgo.GinkgoHelper()
	ctx := ginkgoSuiteCtx()
	resolve := exec.CommandContext(ctx, "ko", "resolve", "-f", "deploy/mecak8s/")
	resolve.Dir = repoRoot()
	resolve.Env = append(resolve.Environ(), "KO_DOCKER_REPO=ko.local")
	resolved, err := resolve.Output()
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred(),
		"ko resolve -f deploy/mecak8s/ failed\n--- output ---\n%s", resolved)

	// Filter out the Kustomization resource (ko emits it; kubectl can't apply it).
	filtered := filterKustomization(resolved)

	// Split into namespace-first + rest: the namespace must be active before
	// namespaced resources can be created.
	namespaceDoc, restDocs := splitNamespace(filtered)
	if namespaceDoc != nil {
		apply := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
		apply.Stdin = bytes.NewReader(namespaceDoc)
		applyOut, err := apply.CombinedOutput()
		gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred(),
			"kubectl apply namespace failed\n--- output ---\n%s", applyOut)
		// Wait for the namespace to be active (the controller sets it asynchronously).
		gomega.Eventually(func() string {
			return runCmdQuiet("kubectl", "get", "namespace", k8sNamespace,
				"-o", "jsonpath={.status.phase}")
		}, 30*time.Second, time.Second).Should(gomega.Equal("Active"),
			"namespace %s did not become Active", k8sNamespace)
	}
	if len(restDocs) > 0 {
		apply := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
		apply.Stdin = bytes.NewReader(restDocs)
		applyOut, err := apply.CombinedOutput()
		gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred(),
			"kubectl apply failed\n--- output ---\n%s", applyOut)
	}
}

// filterKustomization removes any `kind: Kustomization` YAML document from the
// multi-document stream. ko resolve (ko v0.18) emits the kustomization.yaml
// itself as a resource in the output, which kubectl apply rejects as an unknown
// kind (the Kustomization CRD is not installed on a vanilla cluster).
func filterKustomization(input []byte) []byte {
	var out bytes.Buffer
	docs := bytes.Split(input, []byte("\n---\n"))
	for _, doc := range docs {
		if bytes.Contains(doc, []byte("kind: Kustomization")) {
			continue
		}
		if out.Len() > 0 {
			out.Write([]byte("\n---\n"))
		}
		out.Write(doc)
	}
	return out.Bytes()
}

// splitNamespace separates the Namespace document from the rest so the namespace
// can be applied (and waited on) first. Returns (namespaceDoc, restDocs).
func splitNamespace(input []byte) (namespaceDoc, rest []byte) {
	docs := bytes.Split(input, []byte("\n---\n"))
	var restBuf bytes.Buffer
	for _, doc := range docs {
		if bytes.Contains(doc, []byte("kind: Namespace")) {
			namespaceDoc = doc
			continue
		}
		if restBuf.Len() > 0 {
			restBuf.Write([]byte("\n---\n"))
		}
		restBuf.Write(doc)
	}
	return namespaceDoc, restBuf.Bytes()
}

// waitPodsReady waits for every pod in the mecatl namespace carrying the
// part-of=mecak8s label to be Ready. This covers both agent replicas AND the
// Redis pod (both carry the label). The Redis image is pulled from Docker Hub
// on first apply, so the timeout must allow for an image pull.
func waitPodsReady() {
	ginkgo.GinkgoHelper()
	runCmd(ginkgoSuiteCtx(), "kubectl", "wait",
		"--for=condition=Ready", "pod",
		"-n", k8sNamespace,
		"-l", "app.kubernetes.io/part-of="+partOfLabel,
		"--timeout=180s")
}

// podNames returns the names of the agent pods (component=agent), in stable
// sorted order so pod-A / pod-B are addressable across the suite. There are
// exactly two (the Deployment replicas:2); the suite asserts that.
func podNames() []string {
	ginkgo.GinkgoHelper()
	out := runCmd(ginkgoSuiteCtx(), "kubectl", "get", "pods",
		"-n", k8sNamespace,
		"-l", "app.kubernetes.io/component="+agentComponent,
		"-o", "jsonpath={.items[*].metadata.name}")
	names := strings.Fields(strings.TrimSpace(out))
	gomega.ExpectWithOffset(1, names).To(gomega.HaveLen(2),
		"expected exactly two agent pods (replicas:2), got %v", names)
	return names
}

// kubectlDeletePod deletes a pod. Graceful (the default) lets the preStop /drain
// hook + SIGTERM fire, so the pod's Service.Close releases its held leases
// before the TTL. force=true passes --force --grace-period=0, which skips the
// graceful shutdown — NO releaseLease, so the k8s Lease object remains until its
// TTL lapses. The contrast is the failover control case.
func kubectlDeletePod(podName string, force bool) {
	ginkgo.GinkgoHelper()
	args := []string{"delete", "pod", podName, "-n", k8sNamespace}
	if force {
		args = append(args, "--force", "--grace-period=0")
	}
	runCmd(ginkgoSuiteCtx(), "kubectl", args...)
}

// waitReplacementReady waits for a NEW Ready agent pod — one whose name is NOT in
// the preDelete snapshot of pod names that existed before the delete. With
// replicas:2, after deleting pod-A there are briefly two candidates for "a pod
// that is not deletedName": pod-B (the survivor, Ready all along) and the
// genuinely-new replacement. Taking a pre-delete roster and excluding ALL of it
// ensures this returns the NEW replacement pod, never the survivor — which
// matters for the force-delete control case, where the survivor pod-B may have
// just been force-killed and must not be mistaken for its own replacement.
//
// Callers MUST capture the pod names BEFORE calling kubectlDeletePod and pass
// them as preDelete.
func waitReplacementReady(preDelete []string) string {
	ginkgo.GinkgoHelper()
	old := make(map[string]struct{}, len(preDelete))
	for _, n := range preDelete {
		old[n] = struct{}{}
	}
	isNewReady := func(name string) bool {
		if _, hit := old[name]; hit {
			return false // a survivor from the pre-delete roster, not the replacement
		}
		ready := runCmdQuiet("kubectl", "get", "pod", name, "-n", k8sNamespace,
			"-o", "jsonpath={.status.containerStatuses[0].ready}")
		return ready == "true"
	}
	gomega.Eventually(func(g gomega.Gomega) string {
		out := runCmdQuiet("kubectl", "get", "pods",
			"-n", k8sNamespace,
			"-l", "app.kubernetes.io/component="+agentComponent,
			"-o", "jsonpath={.items[*].metadata.name}")
		for _, n := range strings.Fields(strings.TrimSpace(out)) {
			if isNewReady(n) {
				return n
			}
		}
		return ""
	}, 120*time.Second, 2*time.Second).ShouldNot(gomega.BeEmpty(),
		"no NEW (pre-delete-roster-excluded) replacement pod became Ready (pre-delete roster: %v)", preDelete)

	// Re-query to return the stable name (the Eventually closure's return is
	// discarded to keep the matcher simple; re-read once it is known ready).
	for _, n := range strings.Fields(runCmdQuiet("kubectl", "get", "pods",
		"-n", k8sNamespace, "-l", "app.kubernetes.io/component="+agentComponent,
		"-o", "jsonpath={.items[*].metadata.name}")) {
		if isNewReady(n) {
			return n
		}
	}
	ginkgo.Fail("replacement pod became Ready then vanished — race in waitReplacementReady")
	return ""
}

// --- port-forward ------------------------------------------------------------

// portForward starts `kubectl port-forward` from a free local port to the
// agent pod's HTTP listener and returns the local "host:port" address plus a
// stop function that terminates the forward. The forward runs for the lifetime
// of the returned context-cancellation / stop call. The local port is chosen by
// binding a free loopback port first (the same race-free pattern as
// e2e/harness/local.go's freePorts) and handing it to port-forward.
func portForward(podName string) (addr string, stop func()) {
	ginkgo.GinkgoHelper()
	port := freeLocalPort()
	ctx, cancel := context.WithCancel(ginkgoSuiteCtx())
	cmd := exec.CommandContext(ctx, "kubectl", "port-forward",
		"-n", k8sNamespace,
		fmt.Sprintf("pod/%s", podName),
		fmt.Sprintf("%d:%d", port, agentPodPort))
	// port-forward writes progress to stderr; capture it for failure diagnosis.
	var buf ginkgoWriter
	cmd.Stderr = &buf
	gomega.ExpectWithOffset(1, cmd.Start()).To(gomega.Succeed(),
		"start kubectl port-forward for pod %s", podName)

	addr = fmt.Sprintf("127.0.0.1:%d", port)
	// Wait for the forward to be accepting connections before returning, so the
	// caller's first HTTP request does not race the tunnel setup.
	gomega.Eventually(func() bool {
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}, 30*time.Second, 500*time.Millisecond).Should(gomega.BeTrue(),
		"port-forward to pod %s never accepted on %s\n--- port-forward stderr ---\n%s",
		podName, addr, buf.String())

	return addr, func() {
		cancel()
		_ = cmd.Wait()
	}
}

// ginkgoWriter is a thread-safe bytes.Buffer suitable for an exec Cmd's Stderr
// capture (port-forward writes from its own goroutine).
type ginkgoWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (g *ginkgoWriter) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.Write(p)
}

func (g *ginkgoWriter) String() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.String()
}

// freeLocalPort returns a free loopback TCP port. It binds :0, reads the
// assigned port, and closes — the standard ephemeral-port probe. The bind→close
// →hand-to-port-forward window is racy in principle against other processes
// (loopback-private in practice; FlakeAttempts covers the rare collision).
func freeLocalPort() int {
	ginkgo.GinkgoHelper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred(), "bind free local port")
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// --- HTTP API helpers --------------------------------------------------------

// createSessionOverHTTP creates a session via POST /v1/sessions and returns the
// session id. It mirrors the createSessionBody shape (workspace + mode) the
// server's HTTP handler decodes. The workspace is empty (the mock provider does
// not touch the filesystem; a no-fs profile is unnecessary — the default
// profile requires a workspace, so pass "/tmp" which exists in the pod).
func createSessionOverHTTP(ctx context.Context, addr string) string {
	ginkgo.GinkgoHelper()
	body, _ := json.Marshal(map[string]any{
		"workspace": "/tmp",
		"mode":      "default",
	})
	url := fmt.Sprintf("http://%s/v1/sessions", addr)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred(), "POST %s", url)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	gomega.ExpectWithOffset(1, resp.StatusCode).To(gomega.Equal(http.StatusCreated),
		"create session: status %d, body %s", resp.StatusCode, string(raw))
	var respBody struct {
		SessionID string `json:"session_id"`
	}
	gomega.ExpectWithOffset(1, json.Unmarshal(raw, &respBody)).To(gomega.Succeed(),
		"create session: unmarshal %s", string(raw))
	gomega.ExpectWithOffset(1, respBody.SessionID).NotTo(gomega.BeEmpty(), "empty session_id")
	return respBody.SessionID
}

// httpPrompt POSTs a prompt to /v1/sessions/{id}/prompt and returns the HTTP
// status + a bounded slice of the body. A 2xx is an SSE stream; this reads a
// bounded slice (enough to capture a 409 refusal message) — mirroring
// e2e/harness/approve.go's PromptOverHTTP. To drive a run to terminal, use
// drainRun instead.
func httpPrompt(ctx context.Context, addr, sessionID, text string) (status int, body []byte) {
	ginkgo.GinkgoHelper()
	reqBody, _ := json.Marshal(map[string]any{"text": text})
	url := fmt.Sprintf("http://%s/v1/sessions/%s/prompt", addr, sessionID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred(), "POST %s", url)
	defer func() { _ = resp.Body.Close() }()
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 8192))
	return resp.StatusCode, body
}

// drainRun starts a prompt run and drains the SSE stream to completion, returning
// the HTTP status. It is the "drive the run to terminal" helper: the mock
// provider completes a run quickly, and draining ensures the run has fully ended
// (the session reaches a terminal state) before subsequent assertions. A 409
// (lease held elsewhere) returns immediately — there is no stream to drain.
func drainRun(ctx context.Context, addr, sessionID, text string) int {
	ginkgo.GinkgoHelper()
	reqBody, _ := json.Marshal(map[string]any{"text": text})
	url := fmt.Sprintf("http://%s/v1/sessions/%s/prompt", addr, sessionID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred(), "POST %s", url)
	defer func() { _ = resp.Body.Close() }()
	// Drain the SSE stream fully so the run reaches terminal server-side. A 409
	// has a tiny body (the error JSON) and closes immediately. A 2xx streams
	// events until the run ends; read to EOF.
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// httpGetSession GETs /v1/sessions/{id} and returns (status, body). Used to
// prove session persistence across a pod restart (the snapshot lives in Redis,
// so pod-B reads what pod-A wrote).
func httpGetSession(ctx context.Context, addr, sessionID string) (status int, body []byte) {
	ginkgo.GinkgoHelper()
	url := fmt.Sprintf("http://%s/v1/sessions/%s", addr, sessionID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred(), "GET %s", url)
	defer func() { _ = resp.Body.Close() }()
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 8192))
	return resp.StatusCode, body
}

// httpDeleteSession DELETEs /v1/sessions/{id} (CloseSession → releaseLease).
// It is the explicit lease-release lever: a session-scoped lease is held for
// the session's life, and this is what ends it in-process (vs a pod restart,
// which ends it via Service.Close). Returns the HTTP status.
func httpDeleteSession(ctx context.Context, addr, sessionID string) int {
	ginkgo.GinkgoHelper()
	url := fmt.Sprintf("http://%s/v1/sessions/%s", addr, sessionID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred(), "DELETE %s", url)
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// drainRunSoft is drainRun without the hard failure on a transport error: it
// returns the status and ok=false on a dial/HTTP error. It is the Eventually-
// safe probe for takeover assertions where a transient connection blip (the
// survivor mid-acquire) should RETRY, not abort the attempt.
func drainRunSoft(ctx context.Context, addr, sessionID, text string) (status int, ok bool) {
	ginkgo.GinkgoHelper()
	reqBody, _ := json.Marshal(map[string]any{"text": text})
	url := fmt.Sprintf("http://%s/v1/sessions/%s/prompt", addr, sessionID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, true
}

// --- command plumbing --------------------------------------------------------

// ginkgoSuiteCtx returns a context cancelled when the ginkgo suite exits. It is
// the parent for all kubectl/kind/ko commands so a suite abort tears them down.
func ginkgoSuiteCtx() context.Context { return suiteCtx }

// runCmd runs a command under the suite context and fails the spec on a non-zero
// exit, attaching combined output. It is the loud variant for commands whose
// failure is fatal to the spec.
func runCmd(ctx context.Context, name string, args ...string) string {
	ginkgo.GinkgoHelper()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred(),
		"%s %s failed\n--- output ---\n%s", name, strings.Join(args, " "), out)
	return string(out)
}

// runCmdQuiet runs a command and returns its combined output WITHOUT failing on
// a non-zero exit. It is the probe variant — used in Eventually loops where a
// transient failure (pod not yet ready) is expected and retried.
func runCmdQuiet(name string, args ...string) string {
	out, _ := exec.CommandContext(ginkgoSuiteCtx(), name, args...).CombinedOutput()
	return string(out)
}

// repoRoot returns the repository root directory so ko/kubectl commands that
// reference relative paths (./cmd/mecak8s, deploy/mecak8s/) resolve correctly
// regardless of the Go test's working directory.
func repoRoot() string {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "." // fallback: the test's CWD (usually the repo root anyway)
	}
	return strings.TrimSpace(string(out))
}

// formatBody clamps a response body for an assertion message.
func formatBody(b []byte) string {
	if len(b) > 1024 {
		return string(b[:1024]) + "…(truncated)"
	}
	return string(b)
}

// expectLeaseConflict asserts an HTTP 409 response is the LEASE-ELSEWHERE
// refusal, not an unrelated 409. HTTP 409 is returned for two distinct
// conditions — ErrSessionLeasedElsewhere ("server: session is leased by another
// process") AND ErrNoActiveRun — so a bare status==409 check would pass for the
// wrong reason. The handler writes err.Error() as the body, so this verifies the
// body carries the lease signal. Used at every 409 assertion that is meant to
// PROVE lease exclusion (lease_test.go pod-B refusal, failover_test.go force-
// delete control case).
func expectLeaseConflict(status int, body []byte) {
	ginkgo.GinkgoHelper()
	gomega.ExpectWithOffset(1, status).To(gomega.Equal(http.StatusConflict),
		"want HTTP 409 Conflict (lease held elsewhere), got %d\n--- body ---\n%s",
		status, formatBody(body))
	gomega.ExpectWithOffset(1, strings.Contains(string(body), "leased by another process")).
		To(gomega.BeTrue(),
			"409 body does not carry the lease-elsewhere signal (want %q in body)\n--- body ---\n%s",
			"leased by another process", formatBody(body))
}

// leaseTTL is the configured session-lease TTL (30s; the pods run with the
// default --session-lease-ttl, and internal/adapter/k8slease defaults to 30s).
// It bounds how long a force-killed holder's lease blocks a survivor and is the
// timing window the failover control case must observe its 409 WITHIN.
const leaseTTL = 30 * time.Second
