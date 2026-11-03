# Security policy

## Supported versions

Security fixes are applied to the latest released version. Use `paola --version` when reporting an issue.

## Reporting a vulnerability

Do not disclose credentials, private prompts, encrypted history keys, or sensitive project files in a public issue. Use a private GitHub security advisory for this repository when available. If advisories are not enabled, contact the repository maintainer privately before publishing details.

Include the affected version, operating system, reproduction steps, impact, and a minimal proof of concept that contains no secrets.

PAOLA executes local agent CLIs and configured test commands with the permissions available to the invoking user. Review agent configuration and command templates before running them on sensitive repositories.
