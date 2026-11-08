# Security

## Supported versions

This is pre-1.0 software. Only the latest minor release receives security
fixes; at the time of writing that is the v0.5 line. Older minors are not
patched: upgrade to the latest release, reading the changelog for the
breaks on the way. The nested modules (`sqlite`, `postgres`, `kurrentdb`,
`telemetry`) follow the root module's version and are fixed together with
it.

## Reporting a vulnerability

Do not open a public issue or pull request for a vulnerability. Report it
privately through GitHub: open the repository's **Security** tab and choose
**Report a vulnerability**, which opens a draft advisory that only the
maintainer can see.

Describe what the vulnerability is, how to reproduce it, and which versions
and stores you have confirmed it on. We acknowledge reports and coordinate
a fix before disclosure; the fix ships in a release with a changelog entry
and the advisory is published once it is out.
