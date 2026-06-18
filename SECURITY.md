# Security Policy

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

Instead, report them privately through GitHub's built-in mechanism:

1. Go to the **Security** tab of this repository.
2. Click **Report a vulnerability** (this opens a private security advisory).

This keeps the report confidential until a fix is available. If you cannot use
GitHub Security Advisories, contact the maintainer at
`<TODO: security contact email>`.

Please include:

- a description of the issue and its impact,
- steps to reproduce (a minimal proof of concept is ideal),
- affected version / commit, and
- any suggested remediation.

## Scope

Veska is a local daemon that indexes source repositories and serves a code
graph to editors and AI agents over a local MCP socket. Reports of particular
interest include:

- code execution or path-traversal via repository contents or MCP input,
- exposure of indexed data beyond the local socket,
- secrets handling in the bundled secrets scanner.

## Response

We aim to acknowledge a report within a few days, agree on a disclosure
timeline, and credit reporters who wish to be named once a fix ships.
