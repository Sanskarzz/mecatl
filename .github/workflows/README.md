# GitHub Actions workflows for ozzharness

Two workflows live here. Every third-party action is **SHA-pinned** with a
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

## `release.yml` — `v*` tag push

Builds and publishes the `ozzd` image and its supply-chain metadata. The
workflow defaults to `contents: read`; the single publish job elevates to
exactly:

```yaml
permissions:
  contents: read   # checkout
  packages: write  # push image + SBOM to GHCR
  id-token: write  # OIDC: GHCR login AND cosign keyless signing
```

Flow:

1. **Build + push (ko)** — `ko build` straight from `./cmd/ozzd` onto the
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

### Documented follow-up: SLSA provenance

The baseline above gives verifiable origin (keyless signature) and contents
(signed SBOM). Full **SLSA Build L3 provenance** is the documented next step: it
needs the `slsa-framework/slsa-github-generator` container generator as a
separate reusable-workflow job keyed off the built image digest. It is left out
of this first cut because it is a heavier, separately-versioned workflow with
its own permission/secrets contract that warrants its own change and end-to-end
verification. See <https://slsa.dev/>.

## References

- GitHub Actions security hardening — <https://docs.github.com/en/actions/security-guides/security-hardening-for-github-actions>
- OIDC in GitHub Actions — <https://docs.github.com/en/actions/deployment/security-hardening-your-deployments/about-security-hardening-with-openid-connect>
- Preventing pwn requests — <https://securitylab.github.com/resources/github-actions-preventing-pwn-requests/>
- Sigstore / cosign — <https://docs.sigstore.dev/cosign/overview/>
- ko — <https://ko.build/>
- SLSA — <https://slsa.dev/>
