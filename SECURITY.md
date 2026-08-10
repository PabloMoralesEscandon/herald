# Security

Herald is designed as a single-user, local application. Its HTTP API has no
authentication or authorization. Keep `HERALD_HOST=127.0.0.1` unless you place
Herald behind access control that you understand and administer. Binding to a
non-loopback interface exposes reading data and state-changing API operations
to other hosts that can reach the port.

Source URLs are stored locally and included in source exports. Do not share or
commit an export if you added a private or token-bearing feed URL. Herald
rejects credentials in imported URL userinfo, but a provider may put a secret
in a query string.

Do not publish `.herald/`, SQLite databases or sidecars, Obsidian vaults,
exports, `.env` files, logs, or private-key material. The repository
`.gitignore` covers the standard locations and extensions, but review staged
changes before every push.

To report a vulnerability, use the repository host's private security-reporting
feature rather than opening a public issue. Include affected versions,
reproduction steps, impact, and any suggested mitigation. Do not include real
reading data, tokens, or private feed URLs in the report.
