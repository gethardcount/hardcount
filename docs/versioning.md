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
  The next release, v0.9.0, cannot take over a v0.8.x database: you install
  it fresh.

Hardcount has one API version, `/v1`. Before 1.0, a breaking change goes into
`/v1` itself. There is no older version to stay on.

## Write code that survives new values

- Ignore fields you do not know in API answers and webhook payloads.
- Handle an enum value you do not know, for example by logging it, instead
  of failing.
- Accept webhook event types you do not handle, and answer them with a 2xx.

## Where breaking changes are announced

Read these before every upgrade:

1. **The [CHANGELOG](../CHANGELOG.md).** Read the entry for every release
   between yours and the new one. Since 0.4.0, a release with steps to take
   lists them under **Before upgrading**: what changed, who it affects, and
   what to do. A release with nothing to do says so at the top, as 0.6.1
   does. Changes not yet released are under **Unreleased**, where lines for
   code that calls the API say "API callers:".
2. **The release notes** on
   [GitHub releases](https://github.com/gethardcount/hardcount/releases). Since
   v0.6.0, a release with steps to take repeats them under **Before you
   upgrade**. v0.4.0 has them too.

Take a backup before you upgrade. To go back, restore that backup: see
[Upgrading](self-host.md#upgrading).

## Security fixes

Only the latest release gets security fixes. See
[Supported versions](../SECURITY.md#supported-versions).

**Next:** [Self-host](self-host.md).
