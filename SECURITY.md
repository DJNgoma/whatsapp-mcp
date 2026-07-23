# Security policy

## Supported version

Security fixes are applied to the latest revision of `main`. Older revisions are not supported.

## Reporting a vulnerability

Do not open a public issue containing vulnerability details, WhatsApp account identifiers, message content, database files, session data, or media.

Use **Report a vulnerability** on this repository's GitHub **Security** tab. If private vulnerability reporting is temporarily unavailable, contact the maintainer through the repository owner's GitHub profile and request a private reporting channel without including sensitive details in the initial message.

Please use synthetic accounts and data where possible. Include the affected revision, operating system, expected security boundary, and the smallest safe reproduction that demonstrates the issue.

## Deployment boundary

The bridge is designed for a trusted user's local machine and binds to loopback. Do not expose its HTTP port to a LAN or the internet. The API is not a multi-user or remotely authenticated service.
