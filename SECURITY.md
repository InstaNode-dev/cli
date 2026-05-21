# Security Policy

We take the security of the InstaNode CLI seriously. Thank you for helping
keep our users and infrastructure safe.

## Reporting a Vulnerability

Please report suspected security issues by email to **security@instanode.dev**.

Include as much of the following as you can:

- A description of the vulnerability and its impact.
- Steps to reproduce, ideally a minimal proof of concept.
- The affected version, commit SHA, or release tag.
- Any suggested remediation.

We aim to acknowledge new reports within **2 business days** and to provide
a substantive response (triage outcome, severity, expected timeline) within
**7 business days**.

Please do **not** open a public GitHub issue, pull request, or discussion
for suspected vulnerabilities until a fix is available and we have agreed
on a coordinated disclosure date.

## Scope

In scope:

- The CLI binary and source in this repository (`github.com/InstaNode-dev/cli`).
- Authentication flows used by the CLI (device-code login, token storage,
  keyring integration).
- Any code path that handles user credentials, API tokens, or secrets.

Out of scope:

- Vulnerabilities in third-party dependencies (please report upstream; we
  will pick up patched versions promptly).
- Issues that require a pre-compromised local machine (full local user
  account compromise, malicious OS packages, etc.).
- Findings that depend on running custom or modified builds of the CLI.
- Social engineering of InstaNode staff or users.

## Safe Harbor

We will not pursue or support legal action against researchers who:

- Make a good-faith effort to comply with this policy.
- Avoid privacy violations, destruction of data, and disruption of service.
- Only interact with accounts they own or have explicit permission to access.
- Give us a reasonable opportunity to remediate before public disclosure.

If in doubt, email **security@instanode.dev** before testing — we are happy
to scope a test plan with you.
