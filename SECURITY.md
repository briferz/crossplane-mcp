# Security Policy

## Reporting a vulnerability

Please **do not open a public issue** for security vulnerabilities.

Report privately via GitHub's
[**Report a vulnerability**](https://github.com/briferz/crossplane-mcp/security/advisories/new)
(Security → Advisories), or by email to **luis.brfernandez@gmail.com**.

Please include:

- a description of the issue and its impact,
- steps to reproduce or a proof of concept,
- affected version (`crossplane-mcp --version`) and platform.

You can expect an acknowledgement within a few days. We'll keep you updated on
the fix and coordinate disclosure once a patch is available.

## Supported versions

This project is pre-1.0; security fixes are made against the latest release.
Please upgrade to the newest version before reporting.

## Scope notes

`crossplane-mcp` is **read-only** by construction — it only issues
`get`/`list`/`watch` verbs and never mutates cluster state. It talks to the
Kubernetes API using the credentials in your kubeconfig (or in-cluster service
account), so its access is bounded by that identity's RBAC. Granting it a
least-privilege, read-only role is recommended. Connection-secret *contents* are
never returned in tool output.
