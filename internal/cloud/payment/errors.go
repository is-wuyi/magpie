package payment

import "errors"

// Sentinel errors callers compare with errors.Is. Each names the exact
// condition; a database or network failure is returned as its own error and
// is never translated into one of these.
var (
	// ErrInvalidConfig means a Service, Alipay or WeChat configuration
	// value is missing or unusable.
	ErrInvalidConfig = errors.New("payment: invalid configuration")

	// ErrUnknownPreset means no amount preset has the requested id.
	ErrUnknownPreset = errors.New("payment: unknown amount preset")

	// ErrProviderUnavailable means the requested provider is not configured
	// in this Service.
	ErrProviderUnavailable = errors.New("payment: provider is not configured")

	// ErrInvalidAmount means a money value is malformed, zero or above the
	// package's bound. It is never rounded.
	ErrInvalidAmount = errors.New("payment: invalid amount")

	// ErrInvalidOrder means an order is missing a required field or has an
	// unusable one.
	ErrInvalidOrder = errors.New("payment: invalid order")

	// ErrOrderExpired means the order's expiry has passed, so no new payment
	// request may be created for it.
	ErrOrderExpired = errors.New("payment: order has expired")

	// ErrOrderNotFound means no order has that merchant order number.
	ErrOrderNotFound = errors.New("payment: order not found")

	// ErrOrderNotPending means the order already left the pending state and
	// cannot make the requested transition.
	ErrOrderNotPending = errors.New("payment: order is not pending")

	// ErrProviderMismatch means a notification names a provider other than
	// the order's.
	ErrProviderMismatch = errors.New("payment: notification and order providers differ")

	// ErrMerchantMismatch means a notification's merchant identity (Alipay
	// app_id/seller_id, WeChat appid/mchid) is not this deployment's.
	ErrMerchantMismatch = errors.New("payment: notification merchant does not match")

	// ErrCurrencyMismatch means an amount is not in CNY.
	ErrCurrencyMismatch = errors.New("payment: currency is not CNY")

	// ErrAmountMismatch means a notification's amount differs from the
	// stored order's.
	ErrAmountMismatch = errors.New("payment: notification amount does not match the order")

	// ErrTradeNoMismatch means a notification carries a provider trade
	// number different from the one already recorded for the order.
	ErrTradeNoMismatch = errors.New("payment: provider trade number does not match the order")

	// ErrInvalidSignature means a signature is missing, malformed or does
	// not verify. Nothing from a notification that fails this check is
	// trusted or stored.
	ErrInvalidSignature = errors.New("payment: invalid signature")

	// ErrStaleNotification means a WeChat callback timestamp is outside the
	// accepted clock-skew window.
	ErrStaleNotification = errors.New("payment: notification timestamp is stale")

	// ErrUnknownPlatformCertificate means a WeChat callback names a platform
	// certificate serial this deployment does not have, or that certificate
	// is outside its validity window.
	ErrUnknownPlatformCertificate = errors.New("payment: unknown or expired WeChat platform certificate")

	// ErrInvalidNotification means an authenticated notification is
	// malformed or carries a state this package does not understand. It
	// fails closed: nothing is credited.
	ErrInvalidNotification = errors.New("payment: invalid notification")

	// ErrNotificationTooLarge means a callback body exceeds the package's
	// bound.
	ErrNotificationTooLarge = errors.New("payment: notification body is too large")

	// ErrUnsupportedEvent means an authentic notification is not one this
	// package settles, for example a WeChat refund event. The callback
	// endpoint acknowledges it without changing an order.
	ErrUnsupportedEvent = errors.New("payment: unsupported event type")

	// ErrProviderAPI means a provider's HTTP API answered with an error or
	// an unusable body.
	ErrProviderAPI = errors.New("payment: provider API error")

	// ErrInconsistentState means stored payment rows disagree with each
	// other. It is never retried automatically.
	ErrInconsistentState = errors.New("payment: payment state is inconsistent")
)
