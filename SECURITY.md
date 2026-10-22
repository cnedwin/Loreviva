# Security policy

## Scope and maturity

This repository is a development snapshot. Before serving real users, deployment
isolation, authorisation, backup/restore, deletion and monitoring must be verified
independently; existing test records are not production acceptance.

## Reporting a vulnerability

Use **GitHub private vulnerability reporting** on the repository Security page. If
it is unavailable, open an issue that contains **no exploit details** and ask for a
private channel.

Include: affected version or commit, reproduction conditions, expected and actual
behaviour, and a minimal sanitised example. Do not access another person's account,
world, memory or private data to demonstrate an issue.

## What counts as a vulnerability here

- Privilege escalation across tenant, product, subject or world scope
- Reading memory, files or receipts without a current positive-use decision
- Forging a commit receipt, recovery approval or capability claim
- Bypassing a gate with untrusted input (package, document, model output)
- Any path where an operator action is accepted without its grant

## Handling

Maintainers acknowledge within seven days, agree a disclosure date with the
reporter, and credit reporters who wish to be credited. Fixes follow the normal
review path; the regression test lands with the fix.

## Untrusted input

Model output, uploaded documents and community packages are untrusted. API keys
stay server-side; permissions and transactions are enforced by program checks,
never by prompt text. The current build does not call a real model and does not
charge money.

中文版见 [SECURITY.zh-CN.md](SECURITY.zh-CN.md)。
