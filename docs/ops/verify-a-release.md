# Verify a release

This page is for whoever installs or upgrades Hardcount. It shows how to check that a release image was built by this repository's CI from the commit it names, and has not been changed since. It does not check the code itself.

A release has two images: `ghcr.io/gethardcount/hardcount` (the API) and `ghcr.io/gethardcount/hardcount-dashboard`. Run each check for both, with the release tag, such as `v0.9.3`.

| Check | Works for | Needs |
|---|---|---|
| Build provenance | v0.10.0 and later | The GitHub CLI, `gh` |
| Cosign signature | v0.9.0 and later | `cosign` |
| SBOM | v0.10.0 and later | The GitHub CLI, `gh` |

## Build provenance

Each image has a signed record of the workflow run and the commit that built it.

```bash
gh attestation verify oci://ghcr.io/gethardcount/hardcount:<tag> --repo gethardcount/hardcount
gh attestation verify oci://ghcr.io/gethardcount/hardcount-dashboard:<tag> --repo gethardcount/hardcount
```

The command exits with 0 when the image was built by a workflow of `gethardcount/hardcount`, and with 1 when it was not or has no record. Add `--format json` to read the record: `buildSignerURI` is the workflow, and `sourceRepositoryDigest` is the commit.

## Cosign signature

CI signs both images with Cosign, without a stored key: the signature names the workflow that made it. This is the check CI itself runs after signing.

```bash
cosign verify ghcr.io/gethardcount/hardcount:<tag> \
  --certificate-identity "https://github.com/gethardcount/hardcount/.github/workflows/ci.yml@refs/tags/<tag>" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Repeat it with `hardcount-dashboard`. An image that failed its vulnerability scan is never signed, so this check fails for it.

## SBOM

The SBOM lists every package in the API image, in SPDX JSON. It is attached to the GitHub release.

```bash
gh release download <tag> --repo gethardcount/hardcount --pattern hardcount-sbom.spdx.json
```

## Keep the image you checked

A tag can be moved; a digest cannot. To run exactly what you checked, set `HARDCOUNT_IMAGE` and `HARDCOUNT_DASHBOARD_IMAGE` in `.env` to each image's digest, as `ghcr.io/gethardcount/hardcount@sha256:...`. `docker buildx imagetools inspect ghcr.io/gethardcount/hardcount:<tag>` prints it.
