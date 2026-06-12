# GitHub Actions workflows for mecatl

Three workflows live here. Every third-party action is **SHA-pinned** with a
`# vX.Y.Z` comment so a re-pointed tag from a compromised maintainer cannot
silently change what runs. Pins track the Stacklok house set used in
`a downstream consumer`.

## `ci.yml` — push to `main` + every pull request

Runs on `pull_request` (**not** `pull_request_target`): PR code is untrusted and
must run without secrets. `pull_request_target` runs in the base repo's context
*with* secrets — combining it with a checkout of PR code is the classic
"pwn request" RCE pattern, so it is deliberately avoided.

Jobs (each least-privilege at `contents: read`, `timeout-minutes` set,
superseded runs cancelled via `concurrency`):

| Job | What it runs |
|-----|--------------|
| `build` | `go build ./...` |
| `test` | `go test -race ./...` |
| `lint` | `golangci-lint` (v2) + `go vet ./...` |
| `fuzz-smoke` | `task fuzz FUZZTIME=20s` — short coverage-guided pass over the security-critical parsers (not the nightly deep fuzz) |

Go is provisioned by `actions/setup-go` from `go.mod` with the module cache
enabled; `GOTOOLCHAIN=local` prevents a surprise toolchain download.

## `e2e-live.yml` — nightly + manual + `e2e-live` PR label

Runs the **live** e2e suite (`task e2e` → `go test -tags e2e ./e2e/...`, see
`e2e/README.md`): real OpenRouter model calls, real (fractions-of-a-cent)
money. **Non-blocking** — it is not a required check; it exists to catch
live-provider regressions (prompt filters, API drift, model behaviour) the
offline suite cannot see.

Trigger model:

- `schedule` — nightly at 03:17 UTC.
- `workflow_dispatch` — on demand.
- `pull_request` — **only** when a maintainer applies the **`e2e-live`**
  label (`labeled` is in the activity types, so applying the label starts a
  run). The label gate lives in the job `if:` so an unlabelled PR never
  starts the job.

Secret: **`OPENROUTER_API_KEY`** (repo secret, set by the operator). Exposure
is layered:

1. `pull_request`, never `pull_request_target` — fork PR runs get no secrets
   even when labelled.
2. The job `if:` requires `github.repository == 'stacklok/mecatl'`, so forks'
   own scheduled/dispatched runs never reference the secret.
3. A guard step checks key availability and **skips cleanly** (green, with a
   notice) when it is absent — a labelled fork PR or an unconfigured repo
   never hard-fails.
4. The key is injected via `env` into exactly one step, never interpolated
   into a script body or argv.

Note the residual, accepted exposure: a **same-repo** labelled PR runs that
branch's code with the secret in its environment — the label is the
maintainer's explicit opt-in, and same-repo branch authors already have write
access.

Posture: `permissions: contents: read`; one global `concurrency` group
(`e2e-live`, cancel-in-progress) so overlapping dispatches never double the
spend or trip provider rate limits; `timeout-minutes: 30` (the suite runs ~2
minutes; the suite's own layered SpecTimeouts live inside a 60m `go test`
budget, so the job timeout is the cheap runner-billing backstop). On failure
the self-diagnosing transcripts (`.scratch/e2e-*/artifacts/**`, including
`mecated.log`) upload as a 7-day artifact, and the cumulative token/cost
ledger is appended to the job summary when present in the test output.

## `release.yml` — `v*` tag push

Builds and publishes the `mecated` image and its supply-chain metadata. The
workflow defaults to `contents: read`; the single publish job elevates to
exactly:

```yaml
permissions:
  contents: read       # checkout
  packages: write      # push image + SBOM to GHCR
  id-token: write      # OIDC: GHCR login, cosign keyless signing AND provenance
  attestations: write  # SLSA build provenance (actions/attest-build-provenance)
```

Flow:

1. **Build + push (ko)** — `ko build` straight from `./cmd/mecated` onto the
   digest-pinned distroless base in `.ko.yaml`, multi-arch
   (`linux/amd64,linux/arm64`), `--bare`, tagged `<version>` and `latest`.
   `KO_DOCKER_REPO=ghcr.io/${{ github.repository }}`. The image **digest** is
   captured so everything downstream signs the immutable artifact, not a tag.
2. **OIDC registry login** — `ko login ghcr.io` uses the job's `GITHUB_TOKEN`,
   scoped to this repo's packages by `packages: write`. No static registry
   credentials exist in repo secrets.
3. **SBOM** — `ko build --sbom=spdx` pushes an SPDX SBOM next to the image, and
   `anchore/sbom-action` regenerates an `spdx-json` file that is then attached
   as a **signed cosign attestation**.
4. **Keyless signing** — `cosign sign --yes <digest>`. The signing identity is
   this workflow's OIDC token (issuer
   `https://token.actions.githubusercontent.com`), recorded in the public Rekor
   transparency log. No long-lived signing keys.
5. **SLSA build provenance** — `actions/attest-build-provenance` generates a
   signed SLSA provenance predicate for the **image digest** (subject =
   GHCR repo + `sha256:...`, never a tag) and, with `push-to-registry: true`,
   stores it as an OCI referrer of the image. Keyless via the same Sigstore
   (Fulcio/Rekor) machinery — no keys. This binds *how and where* the artifact
   was built to the exact bytes that were published.

### Verifying a signed image (what a consumer runs)

Replace `<owner>/<repo>` and `<tag>`:

```sh
# Verify the keyless signature. The identity is the release workflow's ref.
cosign verify \
  ghcr.io/<owner>/<repo>:<tag> \
  --certificate-identity-regexp '^https://github.com/<owner>/<repo>/\.github/workflows/release\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# Verify the SBOM attestation (same identity flags).
cosign verify-attestation \
  ghcr.io/<owner>/<repo>:<tag> \
  --type spdxjson \
  --certificate-identity-regexp '^https://github.com/<owner>/<repo>/\.github/workflows/release\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# Pin to a digest for production pulls (verify prints the digest):
cosign verify ghcr.io/<owner>/<repo>@sha256:... <flags as above>
```

If you prefer an exact identity over the regexp, use
`--certificate-identity 'https://github.com/<owner>/<repo>/.github/workflows/release.yml@refs/tags/<tag>'`.

### Verifying SLSA build provenance

The provenance attestation is stored as an OCI referrer of the image and is
easiest to verify with the GitHub CLI, which knows the predicate type and the
expected Sigstore identity:

```sh
# Simplest: gh resolves the digest and checks the provenance was produced by
# this repo's workflow.
gh attestation verify \
  oci://ghcr.io/<owner>/<repo>:<tag> \
  --repo <owner>/<repo>
```

Or with cosign, pin to the digest and pass the same keyless identity flags used
for the signature (predicate type `https://slsa.dev/provenance/v1`):

```sh
cosign verify-attestation \
  ghcr.io/<owner>/<repo>@sha256:... \
  --type slsaprovenance1 \
  --certificate-identity-regexp '^https://github.com/<owner>/<repo>/\.github/workflows/release\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

### SLSA provenance — implemented

Full **SLSA build provenance** is now generated for every published image via
GitHub's native `actions/attest-build-provenance`, keyed off the immutable image
digest (step 5 above). The native attestation was chosen over the
`slsa-framework/slsa-github-generator` container generator: it integrates as a
single hardened step in the existing publish job (only `attestations: write`
added), reuses the digest already captured, and stores a verifiable provenance
attestation with the image — no separate, separately-versioned reusable workflow
with its own permission/secrets contract. See <https://slsa.dev/>.

## References

- GitHub Actions security hardening — <https://docs.github.com/en/actions/security-guides/security-hardening-for-github-actions>
- OIDC in GitHub Actions — <https://docs.github.com/en/actions/deployment/security-hardening-your-deployments/about-security-hardening-with-openid-connect>
- Preventing pwn requests — <https://securitylab.github.com/resources/github-actions-preventing-pwn-requests/>
- Sigstore / cosign — <https://docs.sigstore.dev/cosign/overview/>
- ko — <https://ko.build/>
- SLSA — <https://slsa.dev/>
