# Security Policy

## Supported Versions

cheesedb is in preview (before v1.0). Only the latest release gets security fixes.

| Version         | Supported          |
| --------------- | ------------------ |
| Latest release  | :white_check_mark: |
| Older releases  | :x:                |

`cheesedb serve` is a preview and should not be used yet. You can still report problems in it.

## Reporting a Vulnerability

**Please do not open a public issue for security problems.**

Report it privately on GitHub instead:

1. Go to the [Security tab](https://github.com/govinargo-labs/cheesedb/security).
2. Click **Report a vulnerability**.
3. Tell us:
   - what the problem is and what an attacker could do with it
   - the steps to reproduce it (a small test or script is best)
   - the cheesedb version, OS, and how you ran it (library, MCP, CLI, `serve`, Docker)

### What happens next

- We reply within **few days** to confirm we got your report.
- We tell you if we accept it or not, and why.
- If we accept it, we work on a fix and keep you updated. We aim (try our best) to release a fix within **90 days**.
- When the fix is out, we publish a security advisory. We credit you by name, unless you ask us not to.

## Scope

[THREAT_MODEL.md](THREAT_MODEL.md) explains what cheesedb protects against and what it does not.
