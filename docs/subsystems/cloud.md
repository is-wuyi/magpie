# Cloud identity, channels, wallet and payments

Cloud mode (`MAGPIE_CLOUD=1`) is a multi-user service, not the desktop
app's browser mode. It serves only the cloud portal and authenticated
inference/self-service routes. The legacy owner-key page, Agents, Sessions,
Library, Memory, MCP, plugins, backups and host settings are not registered
on the portal and their gateway management paths are refused.

## Sources of truth

PostgreSQL is authoritative. Numbered migrations are embedded in
[`internal/cloud/store/migrations`](../../internal/cloud/store/migrations)
and applied by [`store.Migrate`](../../internal/cloud/store/migrate.go) under
an advisory lock, with checksum and gap checks.

- identity and sessions: [`internal/cloud/store/identity.go`](../../internal/cloud/store/identity.go), [`internal/cloud/auth`](../../internal/cloud/auth);
- providers, house channels, grants and API keys: [`internal/cloud/channels`](../../internal/cloud/channels);
- prices, wallet holds and usage: [`internal/cloud/pricing`](../../internal/cloud/pricing), [`internal/cloud/wallet`](../../internal/cloud/wallet);
- recharge orders and payment events: [`internal/cloud/payment`](../../internal/cloud/payment);
- portal assembly and HTTP authorization: [`internal/gui/cloud.go`](../../internal/gui/cloud.go), [`internal/gui/cloud_payment_http.go`](../../internal/gui/cloud_payment_http.go).

Provider JSON is encrypted as one AES-GCM blob with `MAGPIE_CONFIG_KEY`, so
credential fields cannot accidentally become public metadata. API keys and
email codes are stored only as hashes. A newly created gateway secret is
shown once. `auth` derives code/session HMAC keys from `MAGPIE_AUTH_SECRET`.
SMTP and payment secrets come only from deployment configuration.

`store.json` files and local account discovery are not cloud fallbacks. A
failed provider, identity, authorization or wallet read fails closed.

## Request and authorization path

`startCloudWeb` opens and migrates the store, installs the provider loader,
API-key authenticator and billing services, then starts the gateway. Its
`CloudHandler` separates public auth/payment callbacks, ordinary user APIs
and admin APIs. Every user/admin decision uses the server-side session
principal, not a navigation flag or request-supplied user id.

`access.Check` delegates to the cloud authenticator. Its `Identity` carries
`Cloud`, `UserID` and `HouseChannels`. Loopback is not privileged in cloud
mode. `gateway.providerAllowed` is applied to model lists and every routing
candidate, including fallback and groups. A provider belongs either to one
user or to one house channel. Adding a house channel creates a grant, never a
copy of its credentials. Only published grants are returned by API-key
authentication; offline channels stop working without deleting their usage
or payment history.

Bare model names are resolved against the caller's visible providers before
routing. Provider and routing-group writes invalidate the held catalog
immediately, and unknown-model suggestions name only the caller's authorized
providers and groups. Legacy `/v1/magpie/*`, MCP and local-resource routes are
removed by `cloudRouteGuard`. Self-service usage is always scoped for a cloud
key; it does not depend on the old `heldKey` budget-only condition.

Outbound provider addresses are checked at the actual socket: loopback,
private, link-local and metadata addresses are refused by default, including
after DNS resolution. `MAGPIE_ALLOW_PRIVATE_UPSTREAMS=1` is the
deployment-level escape hatch for operator-controlled VPC upstreams; it is
not a tenant setting.

## Pricing and wallet state

All persisted money is signed `int64` micro-CNY. Token rates are micro-CNY
per million tokens, with separate input, output, cache-read, cache-write and
fixed-per-call components. A zero price is valid only when explicitly
stored. Unknown prices and arithmetic overflow are errors.

A house upstream attempt calls `Reserve` immediately before sending. This
includes chat, embeddings, images, videos, realtime and upstream search, not
only the main Chat handler. Reservations live 24 hours so a long stream is
not refunded while it is still running. The wallet transaction locks the
user's wallet, checks available balance and
writes the reservation, immutable hold entry and price snapshot together.
Each attempt has a unique request id; retries and duplicate calls are
idempotent. BYOK attempts make no wallet reservation.

Usage rows belonging to one attempt, including advisor rows, are aggregated
and settled once at the snapshot price. A failed attempt with no billable
usage is refunded. Settlement can exceed the estimate and leave a negative
balance; new holds then stop until credit arrives. `StaleReservations` finds
expired holds and the portal reconciles them with the usage rows carrying
their reservation ids: billable rows settle, only a confirmed zero-usage
failure refunds, and a missing row, missing log or unreadable log leaves every
unknown hold untouched. A settlement error leaves the durable hold for
reconciliation; it is not logged as a successful charge.

## Recharge orders and callbacks

The user chooses a server-defined preset and provider. The order stores CNY
fen and micro-CNY, a unique merchant order number, pending/success/failed/
expired status and credit state. Alipay uses page-pay RSA2; WeChat Pay uses
API v3 Native, platform-certificate verification and AES-256-GCM resource
decryption. No browser return URL or client-supplied amount settles an order.

A verified callback locks the order, validates merchant/app/order/currency/
amount and moves only `pending → success`. Provider event ids and trade
numbers are unique. Wallet credit uses `payment:<merchant order no>` as its
idempotency key. If crediting after the order commit fails, `credit_state =
'pending'` remains for `SettlePendingCredits`; repeated callbacks cannot
credit twice. Late, duplicate and out-of-order notifications do not
downgrade success or create another credit.

Official merchant integration has no test substitute for a real account.
Offline signature fixtures prove the implemented protocol paths, not live
settlement.

## Verification

```sh
go test ./internal/cloud/...
go test -race ./internal/cloud/auth ./internal/cloud/wallet ./internal/cloud/pricing ./internal/cloud/payment
go test ./internal/gateway -run '^TestCloud'
go test ./internal/gui -run '^(TestEveryRouteIsNamedForTheCLI|TestWebGuard)$'
go test ./internal/cli -run '^TestEveryRouteHasACommandOrIsNamed$'
```

PostgreSQL integration tests are opt-in with `MAGPIE_TEST_DATABASE_URL`.
They create and drop a random schema and require schema-creation privileges;
they are skipped without that variable. GUI tests are in
`internal/gui/tests/cloud-*.test.cjs` and run in Chromium and WebKit. Live
SMTP, Alipay and WeChat checks require deployment credentials and are
reported separately from offline tests.
