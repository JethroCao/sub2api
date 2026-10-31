# Home-managed OpenAI accounts

This integration keeps sub2api's account records, group selection, billing and
direct OpenAI/Codex requests. CLIProxyAPIHome supplies access credentials and
admits model requests against a shared per-credential concurrency limit. CPA
nodes and sub2api nodes use the same Home limit.

```text
sub2api: choose local account in group ── pinned admission / token ──> Home
                   │                                                  ▲
                   └──────── model request ─────────> OpenAI           │
                                                                      │
CPA ───────────────────────────── admission / token ───────────────────┘
 │
 └─────────────────────────────── model request ─────> OpenAI
```

The Home connection is a control-plane connection. Do not change the account's
upstream URL to the Home management URL. Home credential UUIDs, ChatGPT account
IDs and sub2api numeric account IDs are different identifiers.

## Prerequisites

- CLIProxyAPIHome v1.1.7 with a valid credential already imported and a positive
  concurrency policy for every account being bound. For a shared limit of three,
  set the credential's `max_in_flight` to `3` in Home. Sub2's local concurrency can
  further restrict its own traffic; it cannot increase Home's total allowance.
- A separate Home-issued client certificate for **each running sub2api process**.
  Never reuse the certificate of an existing CPA node or another sub2 replica.
- A Home service API key whose credential scope includes only the intended
  accounts and whose model permissions allow the requested upstream models.
  This is distinct from both Home's management key and customer sub2api keys.
- Network access from sub2api to Home's RESP listener, with its TLS certificate
  verifiable against the Home CA. Restrict listener access to intended nodes and
  configure Home's host allowlist as necessary. The public HTTP management
  reverse proxy does not automatically expose the RESP protocol.

Home's certificate management API issues an enrollment JWT through
`POST /v8/management/certificates/clients`. Enroll a newly generated private key
and CSR using Home's certificate enrollment protocol and retain the resulting
client certificate, key and CA. The JWT is an enrollment secret, not the service
API key. Verify the issued CA fingerprint against the enrollment JWT. The
client in this integration consumes PEM files; it does not enroll automatically.
Use Home's provisioning tooling for these files. Keep the private-key directory
private and the key readable only by the sub2 process owner.

## Server configuration

The feature defaults off. Configure the `home` section in sub2api's YAML:

```yaml
home:
  enabled: true
  address: "home.internal.example:8318"
  server_name: "home.internal.example"
  ca_file: "/app/data/home/ca.pem"
  cert_file: "/app/data/home/sub2-client.pem"
  key_file: "/app/data/home/sub2-client-key.pem"
  api_key: "" # Supply HOME_API_KEY through the deployment environment.
```

The host and port above are examples; use the RESP address configured in your
Home deployment. Mount certificate paths into the sub2 container. Supported
environment names are `HOME_ENABLED`, `HOME_ADDRESS`, `HOME_SERVER_NAME`,
`HOME_CA_FILE`, `HOME_CERT_FILE`, `HOME_KEY_FILE` and `HOME_API_KEY`.

Use the server certificate's actual DNS/IP identity for `server_name`. A valid
client certificate alone is insufficient: the server certificate must also
verify. No plaintext/password or certificate-verification bypass is provided.

## Bind existing accounts

Use the account editing form to select Home as the credential source and enter
the corresponding Home credential UUID. Existing local account IDs and group
memberships remain intact. Bind the credential owner for a shadow account rather
than adding independent credentials to the shadow.

The corresponding metadata is:

```json
{
  "openai_credential_source": "home",
  "home_credential_id": "00000000-0000-4000-8000-000000000001"
}
```

The UUID is illustrative. Copy the credential UUID from Home, not the email or
ChatGPT account ID. Submit binding changes through the validated single-account
editing API. Generic Extra, bulk and reauthorization updates cannot be used to
bypass binding validation. Keep the account type as ordinary OpenAI OAuth.

Every actual model attempt is pinned to that UUID. If Home refuses the account,
sub2 cannot silently substitute another Home credential behind the same local
account. Its existing scheduler may select another account permitted by the
original local group and request that account's own pin.

A prepared request retains its Home credential UUID through final admission.
If that binding changes before dispatch, the request fails with a retryable
control-plane error before sending model bytes; any unused admission is released.
Requests prepared with native credentials likewise reject a Home cutover before sending.
Existing WebSocket conversations must reconnect after an ownership change.
Credential recovery does not automatically replay the rejected model attempt.

## Controlled cutover

1. Provision the separate sub2 certificate and scoped Home service key. Configure
   the connection and verify Home membership readiness before routing traffic.
2. In Home, verify the correct credential and apply its shared limit, for example
   `3`. Confirm CPA also uses Home admission for this credential.
3. Start with one existing sub2 OAuth account. Set it inactive and let any
   in-flight model requests and local refresh operations finish. A schedulable
   toggle alone is insufficient to stop every native refresh path.
4. Bind that account to Home through the account editor. Managed accounts no
   longer keep local refresh-capable credentials. Preserve the intended group
   memberships, account ID and proxy settings. Confirm Home still has a valid
   credential after the old refresh work has drained.
5. Test through the account test action and through an existing customer key in
   the intended group. Re-enable the account explicitly after resolving any old
   local error/disabled state; binding does not blanket-reset operator state.
6. Exercise both CPA and sub2 with the same account: at a limit of three, their
   combined active model attempts must not exceed three. Verify successful
   completion, early disconnect and failed requests restore capacity.
7. Repeat for the remaining accounts. Disable duplicate legacy/native paths for
   the same credentials so they cannot independently consume the refresh token
   or bypass Home's concurrency accounting.

An already-issued native OAuth refresh cannot be undone by changing a database
flag. Draining before cutover matters even though the implementation rejects
stale credential writebacks. If the Home refresh token is already invalid,
centralized ownership does not repair it; a valid authorization must first be
restored at Home.

## Runtime behavior and failure handling

Home membership must be ready before a managed model request starts. Access
tokens cached for quota/model metadata reads do not grant inference permission.
When Home is unreachable or the binding/policy is invalid, managed requests fail
closed. Native accounts retain their usual behavior. Disabling `home.enabled`
does not turn bound accounts back into native accounts.

HTTP streams hold their Home slot until upstream work completes or is terminated.
WebSocket model turns need admission even on a reused connection; an idle socket
is not a model request. A lease is released once, independently of usage reports.
Home receives sanitized execution results so its cooldown state reflects sub2
traffic as well as CPA traffic.

Home-managed accounts do not support Live sessions. They are excluded from Live
eligibility, and creation or sideband access rejects Home-backed owners,
including shadow accounts and stale selections, with an explicit unsupported
capability error. Use an eligible native account for Live. After the SDP
exchange, Live media flows directly between the client and upstream; finishing
the SDP HTTP response does not end the session. Home support requires separate
session-lifetime admission and release integration and is not provided by the
current HTTP/WebSocket model-attempt wrappers.

If a dispatch reply is lost, the client cannot know whether Home admitted it. It
fences that membership, cancels local work and reconnects only after Home permits
a fresh membership. This conservative recovery can briefly make managed accounts
unavailable while Home reclaims the old membership. It avoids blindly replaying
an admission or applying an old cumulative release to new work.

Home v1.1.7 has no read-only credential fetch command. Auxiliary credential-cache
misses use its refresh command, which can rotate the token. Refreshes are owned
by Home and coalesced in sub2; model admission remains a separate operation.
Successful refresh-token rotation still depends on the provider accepting Home's
current refresh credential.

Home's authoritative `admitted_in_flight` counter is separate from optional
in-flight observation snapshots. Use the authoritative counter to verify the
shared limit. An observation label such as `fully_enforced: unknown` is not
itself proof of a failed admission policy.

## Rollback

Disabling the integration intentionally stops Home-managed execution. It does
not revive a deleted local refresh token. To restore native ownership, first
deactivate/drain the account, stop the other refresh owner for that credential,
explicitly remove its Home binding through the validated account editor, then
provide a valid independent native OAuth authorization. Do not copy a shared RT
back into two active refresh owners.

Customer API keys and local group memberships need not change when switching an
existing account between these ownership modes.

## Isolated interoperability test

The repository includes a real Home v1.1.7 test harness. Supply a clean checkout
of that tag; the harness verifies source/build identity, starts Home with private
synthetic credentials, enrolls two test certificates and runs the production Go
client against it:

```sh
python3 deploy/tests/home_control_plane_integration.py \
  --home-source /path/to/CLIProxyAPIHome-v1.1.7 \
  --sub2-backend "$PWD/backend"
```

An already-built clean Home binary can be supplied with `--home-binary`. The test
requires Go, Python 3, OpenSSL and permission to bind loopback listeners. The
Home process uses a rejecting loopback proxy for upstream connections and local
model catalogs. It does not use production accounts or call a real model.

The harness checks global capacity across two memberships, exact pinning,
duplicate completion safety, authoritative counters, persisted usage, refresh
without model admission, and fresh membership after shutdown. It requires an
executed passing Go test; a skipped/missing fixture is not accepted as success.
It complements the HTTP/SSE/WS service tests rather than replacing them.

Successful provider refresh-token rotation is outside this isolated test: the
fixture verifies adoption of a newer Home token and keeps upstream network calls
blocked. Production credential validity must be checked during controlled
cutover. The normal backend test suite does not start Home; this package requires
the explicit `homeintegration` build tag and harness-provided configuration.
