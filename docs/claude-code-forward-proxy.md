# Claude Code forward proxy (observe-only)

`claude --cloud` sessions talk to `api.anthropic.com` directly and ignore
`ANTHROPIC_BASE_URL`, so the normal gateway never sees them. This optional
forward proxy lets the WebUI list those sessions. It only observes: nothing is
rewritten, and inference still uses `ANTHROPIC_BASE_URL` as before, provided
that URL bypasses the proxy (set `NO_PROXY` as shown below).

## Enable

1. Add to `config.json` (or use Settings → Cloud in the WebUI) and restart:

   ```json
   "mitm": { "enabled": true }
   ```

   Defaults: `listen` `127.0.0.1:8092` (loopback only, anything else is
   rejected), `registryMax` 1000, `registryTtlMinutes` 1440.

2. Download the CA certificate from Settings → Cloud, or
   `GET /api/mitm/ca.pem`. It is created on first start under
   `<configdir>/mitm/`. The private key (`ca-key.pem`, mode 0600) never leaves
   that directory.

   The CA is valid for 365 days. When it is within 7 days of expiry, or
   `ca.pem` or `ca-key.pem` is missing or unreadable, the next start creates a
   new CA with a new key. Any copy you saved for `NODE_EXTRA_CA_CERTS` then
   stops working and shows up as `TLS trust failures`; download it again and
   compare its fingerprint with the one in Settings → Cloud.

3. Start Claude Code with the proxy and the CA, per process:

   ```sh
   HTTPS_PROXY=http://127.0.0.1:8092 \
   NO_PROXY=127.0.0.1,localhost \
   NODE_EXTRA_CA_CERTS=/path/to/antigravity-proxy-mitm-ca.pem \
   claude --cloud "your task"
   ```

   Use `NODE_EXTRA_CA_CERTS`, which adds a root. Do not use `SSL_CERT_FILE`,
   which replaces the whole root pool. Do not install the CA in the system
   trust store.

   `NO_PROXY` keeps your gateway off the proxy. With a plain `http://`
   `ANTHROPIC_BASE_URL` (the setup in the README), the CLI also sends the
   gateway requests to `HTTPS_PROXY`, and the proxy only speaks CONNECT, so
   they fail with `405` (see Troubleshooting). Add your gateway's host to
   `NO_PROXY` if it is not loopback.

## What it does and does not do

- Decrypts traffic to `anthropic.com`, `claude.ai` and `claude.com` only. The
  CA is name-constrained to those domains. Every other host is a blind tunnel:
  the proxy sees the destination and nothing else.
- Because paths are only visible after TLS is terminated, **all**
  `api.anthropic.com` traffic from a process started with `HTTPS_PROXY` is
  decrypted, including tokens. Keep it loopback-only and leave it off when you
  do not need it.
- Records only a masked route, status and enum-like fields (model, status,
  environment kind) per session, keyed by a 12-character hash. It never stores
  headers, tokens, prompts, titles or raw session ids, and never logs them.
- Upstream requests are forwarded byte for byte over HTTP/1.1 (header case and
  order preserved) with a standard Go TLS handshake. That handshake differs from
  the Claude Code CLI's own TLS stack.

## API

All routes need the WebUI password when one is set.

| Route | Result |
|---|---|
| `GET /api/mitm/status` | enabled, listen address, CA fingerprint and expiry, counters |
| `GET /api/mitm/ca.pem` | CA certificate (404 when disabled) |
| `GET /api/sessions/cloud` | observed sessions, most recent first |
| `GET /api/sessions/cloud/{id}` | one session by its hashed id |

## Troubleshooting

- `TLS trust failures` rising in the WebUI: the process does not trust the CA.
  Check `NODE_EXTRA_CA_CERTS` points at the certificate you downloaded.
- `405 status code (no body)` from the CLI: requests to a plain-`http://`
  `ANTHROPIC_BASE_URL` are going through the proxy, which answers `405` to
  anything but CONNECT. Add the gateway's host to `NO_PROXY` (see step 3), or
  unset the proxy variables for that process.
- `Expect: 100-continue` requests get a 417; the CLI does not send them.
- If the proxy is down while `HTTPS_PROXY` is set, the CLI's Anthropic calls
  fail visibly, the same as when the gateway is down.
- To rotate the CA, stop the proxy, delete `<configdir>/mitm/`, and start it
  again; then re-download the certificate.
