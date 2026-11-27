// Package payment implements cloud recharge orders and the official Alipay
// and WeChat Pay merchant integrations that fund them.
//
// # What an order is
//
// A user picks one of the deployment's amount presets; the server alone
// decides the price. CreateOrder takes only a preset id, a provider and the
// user, never an amount from the client. It persists a pending order with a
// unique merchant order number and then asks the chosen provider for a way to
// pay: an Alipay page-pay URL or a WeChat Pay API v3 Native code URL.
//
// Money is never a float. Orders store the billed amount in fen (CNY cents)
// and the wallet credit in micro-CNY (1 CNY = 1,000,000; 1 fen = 10,000).
// The database keeps the two equal with a CHECK (003_payment.sql).
//
// # How a payment is accepted
//
// HandleAlipayNotify and HandleWeChatNotify verify the provider's signature
// before reading any business field, then check the merchant identity
// (app_id / seller_id for Alipay, appid and mchid for WeChat), the currency,
// the merchant order number and the amount against the stored order. Every
// read and transition of the order runs under its row lock (SELECT ... FOR
// UPDATE); a non-transactional Creditor is called after the success is
// committed, never while that lock is held.
//
// Only a pending order can become success. A success order is credited at
// most once: the unique (provider, provider_event_id) row in payment_events
// makes a replayed callback a no-op, a second provider trade number for the
// same order is refused, and a duplicate callback that arrives while the
// wallet credit is still owed retries that credit with the same idempotency
// key instead of adding money again. A callback for an order that is already
// failed, expired or credited is acknowledged without crediting.
//
// expires_at is not enforced by the callback path: while the stored status is
// still pending, a valid success notification is credited even if the field
// has passed. That is deliberate, because refusing a payment the provider
// accepted would take money without crediting it. Expire is what makes an
// order late, and the caller must close the provider's order before calling
// it.
//
// # Crediting the wallet
//
// Creditor is the storage-independent seam to the wallet:
//
//	Credit(ctx, userUUID, amountMicroCNY, idempotencyKey, description) error
//
// Credit MUST be idempotent on idempotencyKey: the non-transactional path may
// call it again after a crash or a concurrent duplicate, and only the key
// prevents a second ledger row. The wallet service's own Credit returns a
// result as well as an error, so the assembly adapts it without either
// package importing the other:
//
//	payment.CreditorFunc(func(ctx context.Context, userID uuid.UUID, amount int64, key, description string) error {
//		_, err := walletService.Credit(ctx, userID, amount, key, description)
//		return err
//	})
//
// A Creditor that also implements TransactionalCreditor is called with the
// locked transaction itself, so the order's success and the wallet's ledger
// entry commit or roll back together. That is the only way to make the two
// atomic. A Creditor that does not implement it is called after the order
// commits as success with credit_state = 'pending'; a failure leaves that
// state, returns an error to the provider (which retries), and
// SettlePendingCredits can replay it. The deployment should run
// SettlePendingCredits periodically, because a process can die after the
// order commits and before the credit is recorded. The design never marks an
// order credited before its wallet entry is known to be written.
//
// # Storage
//
// PostgresStore is the reference Store. It uses pgx directly and expects the
// tables in internal/cloud/store/migrations/003_payment.sql. Nothing in this
// package opens a pool or reads a DSN; the deployment passes its own
// *pgxpool.Pool.
//
// # What this package does not do
//
// It registers no HTTP routes and performs no authorization. The caller's
// HTTP layer must decide who may create an order, read one (Order returns any
// order by its merchant number), or see a pay URL, and must keep the callback
// endpoints public but signature-verified. A caller maps NotifyResult to the
// provider's acknowledgement: Alipay wants the body "success" only when Ack
// is true, WeChat wants HTTP 200/204 only when Ack is true. A returned error
// means the notification was not settled and the provider should retry; it
// carries no payload, signature or key.
//
// It does not close, query or refund provider orders, does not download the
// WeChat platform certificates (the deployment supplies the current and next
// certificates), and does not handle WeChat refund notifications beyond
// acknowledging them. Alipay's public-key mode is supported (an app private
// key and Alipay's public key), not its certificate mode (app_cert_sn and
// alipay_root_cert_sn). Only WeChat's platform-certificate mode is supported;
// a deployment that chose WeChat's platform public key (a Wechatpay-Serial of
// PUB_KEY_ID_...) must configure the platform certificate instead. Expire
// marks a local order expired; the caller must close the provider order
// first, or a payment made in between is a late callback that needs manual
// reconciliation. No live merchant account has been used to verify the
// protocol implementations; the tests are offline fixtures built from the
// official field definitions.
package payment
