# Versioning before 1.0

Hardcount is pre-1.0. Until 1.0.0, a release can change the API, webhooks,
settings or the database in a way that breaks your setup. This page says
what can change, and where each change is written down before you upgrade.

## What a version number means

Hardcount versions look like `0.MINOR.PATCH`, for example `0.8.2`.

- **A minor release** (`0.8.0`, `0.9.0`) adds features. It can also make
  breaking changes.
- **A patch release** (`0.8.1`, `0.8.2`) is for fixes. A fix can still change
  something you rely on. For example, 0.8.1 made a database upgrade stop
  when it waits more than 5 seconds for a table lock.

## What can break before 1.0

- **API requests and answers.** A field can be renamed, removed or made
  required, a default can change, and a route can answer with a new error.
- **New values.** A field can get a new value, such as `unknown` for a
  credit note's `refund_status`. Webhooks can gain new event types.
- **Settings.** A setting Hardcount used to ignore when it could not read it can
  now stop start-up.
- **The upgrade path.** A release can drop the upgrade from older versions.
  v0.9.0, the first release named Hardcount (the project was called
  Velox), dropped it: it cannot take over a database Velox made, from
  v0.8.x or from a build of `main`, so you install it fresh.

## The API version

Hardcount has one API version, `/v1`. Before 1.0, a breaking change goes into
`/v1` itself. There is no older version to stay on, and no request header
picks a version.

- **The OpenAPI file's `info.version` is a date, `2026-04-07`.** It is the
  day the file was first written. It is not an API version: it does not
  change when the API changes, and you cannot request it.
- **Every JSON answer carries a `Hardcount-Version` header:** the release of
  the server that answered. A release image says its tag, such as `v0.9.1`.
  An image built from `main` says `dev-` and the commit, such as
  `dev-9c4a343`. A server built from source says `dev`. A replayed
  `Idempotency-Key` answer does not carry it.

Use `Hardcount-Version` to tell which release your code is talking to, for
example in your logs.

## Write code that survives new values

- Ignore fields you do not know in API answers and webhook payloads.
- Handle an enum value you do not know, for example by logging it, instead
  of failing.
- Accept webhook event types you do not handle, and answer them with a 2xx.

## Where breaking changes are announced

**There is no notice period before 1.0.** A breaking change can ship in the
next release, minor or patch. It is written in the CHANGELOG when it merges,
so you can see it before you upgrade.

Read these before every upgrade:

1. **The [CHANGELOG](../CHANGELOG.md).** Read the entry for every release
   between yours and the new one. Since 0.4.0, a release with steps to take
   lists them under **Before upgrading**: what changed, who it affects, and
   what to do. A release with nothing to do says so at the top, as 0.6.1
   does. Changes not yet released are under **Unreleased**.
   Lines written for code that calls the API start with **API callers:**.
   To list them, run this in a checkout:

   ```bash
   grep -n 'API callers:' CHANGELOG.md
   ```

   Not every API change has that prefix, so still read each release's whole
   entry.

2. **The release notes** on
   [GitHub releases](https://github.com/gethardcount/hardcount/releases). Since
   v0.6.0, a release with steps to take repeats them under **Before you
   upgrade**. v0.4.0 has them too.

Take a backup before you upgrade. To go back, restore that backup: see
[Going back](upgrades.md#going-back).

## Security fixes

Only the latest release gets security fixes. See
[Supported versions](../SECURITY.md#supported-versions).

**Next:** [Errors](api/errors.md), to handle a new value or code safely.
