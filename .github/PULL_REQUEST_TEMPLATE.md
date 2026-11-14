<!-- Delete what does not apply. -->

## Linked issue

<!-- The issue this closes, if there is one. Only collaborators can open a pull request: see CONTRIBUTING, "What we accept". -->
Closes #

## What changed, why, and how I checked it

## Checklist

<!-- CI runs more checks: see CONTRIBUTING, "What CI runs on your PR". -->
- [ ] It is one change, with no unrelated edits.
- [ ] I ran it, and the description says how I checked.
- [ ] I can explain every line. Commits an AI tool helped with end in `Assisted-by:`.
- [ ] Tests cover the new behavior.
- [ ] `go test ./... -race -short -count=1` and `make lint` pass.
- [ ] A user-visible change has a `CHANGELOG.md` entry and updated docs.
- [ ] A UI change has a Playwright spec, and I looked at a screenshot.
- [ ] `api/openapi.yaml` changed: I ran `make gen` and committed the result.
- [ ] `web-v2/` changed: `cd web-v2 && npm run build && npm test && npm run lint` passes.

## Site-set (money and billing state only)

<!-- Every place that writes, reacts to or branches on the state you change, with the search you ran. Then each checklist item in docs/dev/money-path-robustness-playbook.md: yes, no, or "n/a, because ...". -->
