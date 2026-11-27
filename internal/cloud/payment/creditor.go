package payment

import (
	"context"

	"github.com/google/uuid"
)

// Creditor adds a recharge to the user's spendable balance. The payment
// package defines the interface, so it never imports the wallet package and
// the wallet never needs to import this one.
//
// Credit MUST be idempotent on idempotencyKey. The non-transactional callback
// path calls it outside the order's database transaction (never while holding
// the row lock), so a crash, a concurrent duplicate or a retry can call it
// again with the same key. A wallet that records one ledger row per key
// satisfies this; one that does not would double-credit on a replay.
//
// amountMicroCNY is always positive and within a recharge's bound, and
// userUUID is the order's owner from payment_orders, not a client value.
type Creditor interface {
	Credit(ctx context.Context, userUUID uuid.UUID, amountMicroCNY int64, idempotencyKey, description string) error
}

// TransactionalCreditor is the optional stronger form. When the Creditor
// implements it, the callback calls CreditTx inside the same PostgreSQL
// transaction that holds the order's row lock. The wallet ledger row and the
// order's success/credited state then commit or roll back together, which is
// the only way to make them fully atomic; CreditTx must not perform network
// I/O while it holds that transaction.
//
// A Creditor that does not implement this interface is used through Credit
// and the credit_state = 'pending' recovery path. Deployments that need the
// atomic guarantee should implement this method (for example by sharing the
// wallet's ledger insert through tx.Exec/QueryRow or tx.Raw) and pass it as
// the Service's Creditor. CreditTx must not perform network I/O while it
// holds that transaction.
type TransactionalCreditor interface {
	CreditTx(ctx context.Context, tx Tx, userUUID uuid.UUID, amountMicroCNY int64, idempotencyKey, description string) error
}

// CreditorFunc adapts a function to Creditor. It is how an existing wallet
// whose Credit returns a result as well as an error is plugged in without
// either package importing the other:
//
//	payment.CreditorFunc(func(ctx context.Context, userID uuid.UUID, amount int64, key, description string) error {
//		_, err := walletService.Credit(ctx, userID, amount, key, description)
//		return err
//	})
type CreditorFunc func(ctx context.Context, userUUID uuid.UUID, amountMicroCNY int64, idempotencyKey, description string) error

// Credit calls f.
func (f CreditorFunc) Credit(ctx context.Context, userUUID uuid.UUID, amountMicroCNY int64, idempotencyKey, description string) error {
	return f(ctx, userUUID, amountMicroCNY, idempotencyKey, description)
}
