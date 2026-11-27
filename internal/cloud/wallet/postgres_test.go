package wallet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yetone/magpie/internal/cloud/pricing"
)

// These tests need a real PostgreSQL database. They are opt-in: without
// MAGPIE_TEST_DATABASE_URL they skip, and no test in this file reaches the
// network or a developer's database. Each test creates its own schema, applies
// 002_billing.sql there, and drops the schema afterwards.
//
// 001_identity.sql belongs to another change; the test schema gets a minimal
// users(id uuid) fixture because billing only references that column.

const testUserTable = `CREATE TABLE users (id uuid PRIMARY KEY)`

type testDB struct {
	dsn    string
	schema string
	pool   *pgxpool.Pool
	svc    *Service
}

func newTestDB(t *testing.T) *testDB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("MAGPIE_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("set MAGPIE_TEST_DATABASE_URL to run PostgreSQL wallet integration tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to MAGPIE_TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = admin.Close(closeCtx)
	})

	schema := "magpie_wallet_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.Exec(dropCtx, "DROP SCHEMA IF EXISTS "+ident+" CASCADE"); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})
	if _, err := admin.Exec(ctx, "SET search_path TO "+ident); err != nil {
		t.Fatalf("set search_path: %v", err)
	}
	if _, err := admin.Exec(ctx, testUserTable); err != nil {
		t.Fatalf("create users fixture: %v", err)
	}
	migration, err := os.ReadFile(filepath.Join("..", "store", "migrations", "002_billing.sql"))
	if err != nil {
		t.Fatalf("read 002_billing.sql: %v", err)
	}
	if _, err := admin.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("apply 002_billing.sql: %v", err)
	}

	db := &testDB{dsn: dsn, schema: schema}
	db.pool = newTestPool(t, dsn, schema)
	db.svc, err = New(db.pool)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return db
}

func newTestPool(t *testing.T, dsn, schema string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse MAGPIE_TEST_DATABASE_URL: %v", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = make(map[string]string)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 8
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("create test pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newService is a restart-like second process: a new pool and a new Service on
// the same database state.
func (db *testDB) newService(t *testing.T) *Service {
	t.Helper()
	pool := newTestPool(t, db.dsn, db.schema)
	svc, err := New(pool)
	if err != nil {
		t.Fatalf("New second service: %v", err)
	}
	return svc
}

func (db *testDB) newUser(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.pool.Exec(context.Background(),
		`INSERT INTO users (id) VALUES ($1::uuid)`, id.String()); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id
}

func testQuote(t *testing.T) pricing.Quote {
	t.Helper()
	quote, err := pricing.NewQuote(
		pricing.Key{Channel: "house", Provider: "openai", Model: "gpt-5"},
		pricing.Price{
			InputPerMTok:      3_000_000,
			OutputPerMTok:     15_000_000,
			CacheReadPerMTok:  300_000,
			CacheWritePerMTok: 3_750_000,
			FixedPerCall:      1_000,
		})
	if err != nil {
		t.Fatalf("test quote: %v", err)
	}
	return quote
}

func estimatedUsage() pricing.Usage {
	return pricing.Usage{
		InputTokens:      1_000,
		OutputTokens:     500,
		CacheReadTokens:  2_000,
		CacheWriteTokens: 1_000,
	}
}

func actualUsage() pricing.Usage {
	return pricing.Usage{
		InputTokens:      800,
		OutputTokens:     400,
		CacheReadTokens:  1_000,
		CacheWriteTokens: 500,
	}
}

func mustCredit(t *testing.T, svc *Service, user uuid.UUID, amount int64, key string) CreditResult {
	t.Helper()
	result, err := svc.Credit(context.Background(), user, amount, key, "test credit")
	if err != nil {
		t.Fatalf("Credit(%s, %d, %q): %v", user, amount, key, err)
	}
	return result
}

func mustReserve(t *testing.T, svc *Service, user uuid.UUID, requestID string, quote pricing.Quote, usage pricing.Usage) Reservation {
	t.Helper()
	res, err := svc.Reserve(context.Background(), ReserveRequest{
		UserID:         user,
		RequestID:      requestID,
		Quote:          quote,
		EstimatedUsage: usage,
		Description:    "test request",
	})
	if err != nil {
		t.Fatalf("Reserve(%q): %v", requestID, err)
	}
	return res
}

func mustBalance(t *testing.T, svc *Service, user uuid.UUID) Balance {
	t.Helper()
	balance, err := svc.Balance(context.Background(), user)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	return balance
}

func (db *testDB) countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

func (db *testDB) countEntries(t *testing.T, user uuid.UUID, kind string) int {
	t.Helper()
	return db.countRows(t,
		`SELECT count(*) FROM wallet_entries WHERE user_id = $1::uuid AND kind = $2`,
		user.String(), kind)
}

func (db *testDB) countReservations(t *testing.T, user uuid.UUID) int {
	t.Helper()
	return db.countRows(t,
		`SELECT count(*) FROM billing_reservations WHERE user_id = $1::uuid`, user.String())
}

func checkMoney(t *testing.T, label string, got, want pricing.Micros) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %d, want %d", label, got, want)
	}
}

func TestPostgresCreditReserveSettle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := db.newUser(t)

	zero := mustBalance(t, db.svc, user)
	checkMoney(t, "initial balance", zero.BalanceMicros, 0)
	checkMoney(t, "initial held", zero.HeldMicros, 0)

	credit := mustCredit(t, db.svc, user, 10_000_000, "topup-1")
	checkMoney(t, "credit amount", credit.AmountMicros, 10_000_000)
	checkMoney(t, "credit balance", credit.BalanceMicros, 10_000_000)
	if credit.Replayed {
		t.Fatal("first credit reported itself as a replay")
	}

	quote := testQuote(t)
	const hold = pricing.Micros(15_850)
	res := mustReserve(t, db.svc, user, "req-1", quote, estimatedUsage())
	if res.Status != StatusHeld {
		t.Fatalf("reservation status = %q, want held", res.Status)
	}
	checkMoney(t, "hold", res.HoldMicros, hold)
	checkMoney(t, "charged before settle", res.ChargedMicros, 0)
	if res.Replayed {
		t.Fatal("first reserve reported itself as a replay")
	}
	if res.Snapshot.Key != quote.Key || res.Snapshot.CapturedAt.IsZero() {
		t.Fatalf("reservation snapshot = %+v", res.Snapshot)
	}

	balance := mustBalance(t, db.svc, user)
	checkMoney(t, "balance after hold", balance.BalanceMicros, 10_000_000)
	checkMoney(t, "held after hold", balance.HeldMicros, hold)
	checkMoney(t, "available after hold", balance.AvailableMicros, 10_000_000-hold)

	const charge = pricing.Micros(11_575)
	settlement, err := db.svc.Settle(ctx, SettleRequest{
		ReservationID: res.ID,
		ActualUsage:   actualUsage(),
	})
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	checkMoney(t, "settled charge", settlement.ChargedMicros, charge)
	checkMoney(t, "released hold", settlement.ReleasedMicros, hold)
	checkMoney(t, "unused hold", settlement.UnusedMicros, hold-charge)
	checkMoney(t, "balance after settle", settlement.BalanceMicros, 10_000_000-charge)
	if settlement.Replayed {
		t.Fatal("first settle reported itself as a replay")
	}

	balance = mustBalance(t, db.svc, user)
	checkMoney(t, "balance after settle", balance.BalanceMicros, 10_000_000-charge)
	checkMoney(t, "held after settle", balance.HeldMicros, 0)

	replay, err := db.svc.Settle(ctx, SettleRequest{
		ReservationID: res.ID,
		ActualUsage:   pricing.Usage{InputTokens: 99_999_999},
	})
	if err != nil {
		t.Fatalf("replayed Settle: %v", err)
	}
	if !replay.Replayed {
		t.Fatal("replayed settle did not report Replayed")
	}
	checkMoney(t, "replayed charge", replay.ChargedMicros, charge)
	checkMoney(t, "replayed balance", replay.BalanceMicros, 10_000_000-charge)
	if replay.EntryID != settlement.EntryID {
		t.Fatalf("replayed entry = %s, want %s", replay.EntryID, settlement.EntryID)
	}
	balance = mustBalance(t, db.svc, user)
	checkMoney(t, "balance after replayed settle", balance.BalanceMicros, 10_000_000-charge)

	if got := db.countEntries(t, user, "hold"); got != 1 {
		t.Fatalf("hold entries = %d, want 1", got)
	}
	if got := db.countEntries(t, user, "settle"); got != 1 {
		t.Fatalf("settle entries = %d, want 1", got)
	}
	if got := db.countReservations(t, user); got != 1 {
		t.Fatalf("reservations = %d, want 1", got)
	}
	if got := db.countRows(t, `SELECT count(*) FROM usage_records WHERE user_id = $1::uuid`, user.String()); got != 1 {
		t.Fatalf("usage records = %d, want 1", got)
	}
	var input, output, cacheRead, cacheWrite, cost int64
	if err := db.pool.QueryRow(ctx,
		`SELECT input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_micros
		 FROM usage_records WHERE user_id = $1::uuid`, user.String()).
		Scan(&input, &output, &cacheRead, &cacheWrite, &cost); err != nil {
		t.Fatalf("read usage record: %v", err)
	}
	if input != 800 || output != 400 || cacheRead != 1_000 || cacheWrite != 500 || cost != int64(charge) {
		t.Fatalf("usage record = (%d,%d,%d,%d,%d), want (800,400,1000,500,%d)",
			input, output, cacheRead, cacheWrite, cost, charge)
	}

	// Reserving the same request again after settlement returns the settled
	// reservation and takes no second hold.
	again, err := db.svc.Reserve(ctx, ReserveRequest{
		UserID:         user,
		RequestID:      "req-1",
		Quote:          quote,
		EstimatedUsage: pricing.Usage{InputTokens: 1_000_000_000},
	})
	if err != nil {
		t.Fatalf("Reserve replay after settle: %v", err)
	}
	if !again.Replayed || again.ID != res.ID || again.Status != StatusSettled {
		t.Fatalf("reserve replay = %+v", again)
	}
	checkMoney(t, "replayed hold", again.HoldMicros, hold)
	checkMoney(t, "replayed charged", again.ChargedMicros, charge)

	if _, err := db.svc.Refund(ctx, RefundRequest{ReservationID: res.ID}); !errors.Is(err, ErrReservationSettled) {
		t.Fatalf("Refund after settle error = %v, want ErrReservationSettled", err)
	}

	// The ledger sums to the wallet's stored totals.
	var ledgerBalance, ledgerHeld int64
	if err := db.pool.QueryRow(ctx,
		`SELECT COALESCE(sum(balance_delta_micros), 0), COALESCE(sum(held_delta_micros), 0)
		 FROM wallet_entries WHERE user_id = $1::uuid`, user.String()).
		Scan(&ledgerBalance, &ledgerHeld); err != nil {
		t.Fatalf("sum ledger: %v", err)
	}
	if ledgerBalance != 10_000_000-int64(charge) || ledgerHeld != 0 {
		t.Fatalf("ledger totals = (%d,%d), want (%d,0)", ledgerBalance, ledgerHeld, 10_000_000-int64(charge))
	}
}

func TestPostgresParallelReserveSpendsOneHold(t *testing.T) {
	db := newTestDB(t)
	user := db.newUser(t)
	mustCredit(t, db.svc, user, 100_000, "parallel-topup")

	quote, err := pricing.NewQuote(
		pricing.Key{Provider: "openai", Model: "fixed-fee"},
		pricing.Price{FixedPerCall: 100_000})
	if err != nil {
		t.Fatalf("quote: %v", err)
	}

	const callers = 8
	type outcome struct {
		res Reservation
		err error
	}
	results := make(chan outcome, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := db.svc.Reserve(context.Background(), ReserveRequest{
				UserID:    user,
				RequestID: fmt.Sprintf("parallel-%d", i),
				Quote:     quote,
			})
			results <- outcome{res: res, err: err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	successes, insufficient := 0, 0
	for result := range results {
		switch {
		case result.err == nil:
			successes++
			checkMoney(t, "parallel hold", result.res.HoldMicros, 100_000)
		case errors.Is(result.err, ErrInsufficientFunds):
			insufficient++
		default:
			t.Errorf("unexpected Reserve error: %v", result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful reserves = %d, want exactly 1", successes)
	}
	if insufficient != callers-1 {
		t.Fatalf("insufficient reserves = %d, want %d", insufficient, callers-1)
	}

	balance := mustBalance(t, db.svc, user)
	checkMoney(t, "balance", balance.BalanceMicros, 100_000)
	checkMoney(t, "held", balance.HeldMicros, 100_000)
	checkMoney(t, "available", balance.AvailableMicros, 0)
	if got := db.countReservations(t, user); got != 1 {
		t.Fatalf("reservations = %d, want 1", got)
	}
}

func TestPostgresRefundIsIdempotentAcrossServices(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := db.newUser(t)
	mustCredit(t, db.svc, user, 50_000, "refund-topup")

	res := mustReserve(t, db.svc, user, "refund-req", testQuote(t), estimatedUsage())
	second := db.newService(t)

	type result struct {
		refund RefundResult
		err    error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, svc := range []*Service{db.svc, second} {
		wg.Add(1)
		go func(svc *Service) {
			defer wg.Done()
			<-start
			refund, err := svc.Refund(context.Background(), RefundRequest{
				ReservationID: res.ID,
				Reason:        "client cancelled",
			})
			results <- result{refund: refund, err: err}
		}(svc)
	}
	close(start)
	wg.Wait()
	close(results)

	firsts, replays := 0, 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent Refund: %v", result.err)
		}
		if result.refund.Replayed {
			replays++
		} else {
			firsts++
			checkMoney(t, "refund released", result.refund.ReleasedMicros, 15_850)
		}
		checkMoney(t, "refund balance", result.refund.BalanceMicros, 50_000)
	}
	if firsts != 1 || replays != 1 {
		t.Fatalf("refunds first/replayed = %d/%d, want 1/1", firsts, replays)
	}

	balance := mustBalance(t, db.svc, user)
	checkMoney(t, "balance after refund", balance.BalanceMicros, 50_000)
	checkMoney(t, "held after refund", balance.HeldMicros, 0)
	if got := db.countEntries(t, user, "refund"); got != 1 {
		t.Fatalf("refund entries = %d, want 1", got)
	}

	replay, err := second.Refund(ctx, RefundRequest{ReservationID: res.ID})
	if err != nil {
		t.Fatalf("sequential refund replay: %v", err)
	}
	if !replay.Replayed {
		t.Fatal("sequential refund did not report Replayed")
	}
	checkMoney(t, "sequential replay released", replay.ReleasedMicros, 15_850)

	if _, err := db.svc.Settle(ctx, SettleRequest{ReservationID: res.ID}); !errors.Is(err, ErrReservationRefunded) {
		t.Fatalf("Settle after refund error = %v, want ErrReservationRefunded", err)
	}
}

func TestPostgresSettleIsIdempotentAcrossServices(t *testing.T) {
	db := newTestDB(t)
	user := db.newUser(t)
	mustCredit(t, db.svc, user, 50_000, "settle-topup")

	res := mustReserve(t, db.svc, user, "settle-req", testQuote(t), estimatedUsage())
	second := db.newService(t)

	type result struct {
		settlement Settlement
		err        error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, svc := range []*Service{db.svc, second} {
		wg.Add(1)
		go func(i int, svc *Service) {
			defer wg.Done()
			usage := actualUsage()
			if i == 1 {
				// Different usage on purpose: the replay reports the first
				// settlement and must not charge this one too.
				usage = pricing.Usage{}
			}
			settlement, err := svc.Settle(context.Background(), SettleRequest{
				ReservationID: res.ID,
				ActualUsage:   usage,
			})
			results <- result{settlement: settlement, err: err}
		}(i, svc)
	}
	close(start)
	wg.Wait()
	close(results)

	firsts, replays := 0, 0
	var charged pricing.Micros
	var entryID uuid.UUID
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent Settle: %v", result.err)
		}
		if result.settlement.Replayed {
			replays++
		} else {
			firsts++
			charged = result.settlement.ChargedMicros
			entryID = result.settlement.EntryID
		}
	}
	if firsts != 1 || replays != 1 {
		t.Fatalf("settles first/replayed = %d/%d, want 1/1", firsts, replays)
	}
	if charged != 11_575 && charged != 1_000 {
		t.Fatalf("charged = %d, want one of the two usages", charged)
	}

	balance := mustBalance(t, db.svc, user)
	checkMoney(t, "balance after one settle", balance.BalanceMicros, 50_000-charged)
	checkMoney(t, "held after one settle", balance.HeldMicros, 0)
	if got := db.countEntries(t, user, "settle"); got != 1 {
		t.Fatalf("settle entries = %d, want 1", got)
	}
	if got := db.countRows(t, `SELECT count(*) FROM usage_records WHERE user_id = $1::uuid`, user.String()); got != 1 {
		t.Fatalf("usage records = %d, want 1", got)
	}
	replay, err := second.Settle(context.Background(), SettleRequest{ReservationID: res.ID, ActualUsage: actualUsage()})
	if err != nil {
		t.Fatalf("sequential settle replay: %v", err)
	}
	if !replay.Replayed || replay.EntryID != entryID {
		t.Fatalf("sequential replay = %+v, want entry %s", replay, entryID)
	}
	checkMoney(t, "sequential replay charge", replay.ChargedMicros, charged)
}

func TestPostgresWalletEntriesAreAppendOnly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := db.newUser(t)
	mustCredit(t, db.svc, user, 1_000, "append-only-topup")

	for _, statement := range []string{
		`UPDATE wallet_entries SET balance_after_micros = 0 WHERE user_id = $1::uuid`,
		`DELETE FROM wallet_entries WHERE user_id = $1::uuid`,
	} {
		if _, err := db.pool.Exec(ctx, statement, user.String()); err == nil {
			t.Fatalf("%q was allowed on wallet_entries", statement)
		}
	}
	if _, err := db.pool.Exec(ctx, `TRUNCATE wallet_entries`); err == nil {
		t.Fatal("TRUNCATE was allowed on wallet_entries")
	}

	if got := db.countEntries(t, user, "credit"); got != 1 {
		t.Fatalf("credit entries = %d, want 1", got)
	}
	checkMoney(t, "balance after refused changes", mustBalance(t, db.svc, user).BalanceMicros, 1_000)
}

func TestPostgresCreditReplayAndConflict(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	userA := db.newUser(t)
	userB := db.newUser(t)
	second := db.newService(t)

	first := mustCredit(t, db.svc, userA, 1_000, "pay-1")
	replay, err := second.Credit(ctx, userA, 1_000, "pay-1", "retried callback")
	if err != nil {
		t.Fatalf("replayed Credit: %v", err)
	}
	if !replay.Replayed {
		t.Fatal("replayed credit did not report Replayed")
	}
	if replay.EntryID != first.EntryID {
		t.Fatalf("replayed entry = %s, want %s", replay.EntryID, first.EntryID)
	}
	checkMoney(t, "replayed amount", replay.AmountMicros, 1_000)
	checkMoney(t, "replayed balance", replay.BalanceMicros, 1_000)

	if _, err := db.svc.Credit(ctx, userB, 500, "pay-1", ""); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("same key other user error = %v, want ErrIdempotencyConflict", err)
	}
	checkMoney(t, "other user balance", mustBalance(t, db.svc, userB).BalanceMicros, 0)

	mustCredit(t, db.svc, userA, 500, "pay-2")

	// Concurrent replays of one key credit once; every caller succeeds.
	const callers = 4
	const amount = 700
	results := make(chan error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := db.svc.Credit(context.Background(), userA, amount, "pay-concurrent", "")
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent Credit: %v", err)
		}
	}
	checkMoney(t, "balance after concurrent credit", mustBalance(t, db.svc, userA).BalanceMicros, 1_000+500+amount)
	if got := db.countRows(t,
		`SELECT count(*) FROM wallet_entries WHERE idempotency_key = $1`, "pay-concurrent"); got != 1 {
		t.Fatalf("entries for pay-concurrent = %d, want 1", got)
	}
	if got := db.countEntries(t, userA, "credit"); got != 3 {
		t.Fatalf("credit entries = %d, want 3", got)
	}
}

func TestPostgresNewServiceSeesAndFinishesWork(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := db.newUser(t)

	first := db.newService(t)
	mustCredit(t, first, user, 10_000_000, "restart-topup")
	res := mustReserve(t, first, user, "restart-req", testQuote(t), estimatedUsage())

	second := db.newService(t)
	balance := mustBalance(t, second, user)
	checkMoney(t, "balance seen by second service", balance.BalanceMicros, 10_000_000)
	checkMoney(t, "held seen by second service", balance.HeldMicros, 15_850)

	settlement, err := second.Settle(ctx, SettleRequest{
		ReservationID: res.ID,
		ActualUsage:   actualUsage(),
	})
	if err != nil {
		t.Fatalf("second service Settle: %v", err)
	}
	checkMoney(t, "charge by second service", settlement.ChargedMicros, 11_575)

	third := db.newService(t)
	balance = mustBalance(t, third, user)
	checkMoney(t, "balance seen by third service", balance.BalanceMicros, 10_000_000-11_575)
	checkMoney(t, "held seen by third service", balance.HeldMicros, 0)

	replay, err := third.Settle(ctx, SettleRequest{ReservationID: res.ID, ActualUsage: actualUsage()})
	if err != nil {
		t.Fatalf("third service Settle replay: %v", err)
	}
	if !replay.Replayed {
		t.Fatal("third service settle did not report Replayed")
	}
	checkMoney(t, "third service replay charge", replay.ChargedMicros, 11_575)

	// The payment key recorded before the restart still replays.
	topup, err := third.Credit(ctx, user, 10_000_000, "restart-topup", "after restart")
	if err != nil {
		t.Fatalf("credit replay after restart: %v", err)
	}
	if !topup.Replayed {
		t.Fatal("credit after restart did not report Replayed")
	}
	checkMoney(t, "balance after credit replay", mustBalance(t, third, user).BalanceMicros, 10_000_000-11_575)
}

func TestPostgresRecoverStaleHolds(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := db.newUser(t)
	mustCredit(t, db.svc, user, 20_000, "stale-topup")

	if _, err := db.svc.Reserve(ctx, ReserveRequest{
		UserID:         user,
		RequestID:      "stale-req",
		Quote:          testQuote(t),
		EstimatedUsage: estimatedUsage(),
		TTL:            time.Hour,
	}); err != nil {
		t.Fatalf("Reserve with TTL: %v", err)
	}

	before := mustBalance(t, db.svc, user)
	checkMoney(t, "held before expiry", before.HeldMicros, 15_850)

	recovered, err := db.svc.RecoverStaleHolds(ctx, time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("RecoverStaleHolds before expiry: %v", err)
	}
	if recovered.Reservations != 0 {
		t.Fatalf("recovered before expiry = %d, want 0", recovered.Reservations)
	}
	checkMoney(t, "held before expiry", mustBalance(t, db.svc, user).HeldMicros, 15_850)

	recovered, err = db.svc.RecoverStaleHolds(ctx, time.Now().Add(2*time.Hour), 10)
	if err != nil {
		t.Fatalf("RecoverStaleHolds after expiry: %v", err)
	}
	if recovered.Reservations != 1 {
		t.Fatalf("recovered after expiry = %d, want 1", recovered.Reservations)
	}
	checkMoney(t, "recovered money", recovered.ReleasedMicros, 15_850)

	after := mustBalance(t, db.svc, user)
	checkMoney(t, "balance after recovery", after.BalanceMicros, 20_000)
	checkMoney(t, "held after recovery", after.HeldMicros, 0)

	recovered, err = db.svc.RecoverStaleHolds(ctx, time.Now().Add(2*time.Hour), 10)
	if err != nil {
		t.Fatalf("second RecoverStaleHolds: %v", err)
	}
	if recovered.Reservations != 0 {
		t.Fatalf("second recovery = %d, want 0", recovered.Reservations)
	}

	// A settled reservation is not released by a later recovery.
	settled := mustReserve(t, db.svc, user, "stale-settled", testQuote(t), estimatedUsage())
	if _, err := db.svc.Settle(ctx, SettleRequest{ReservationID: settled.ID, ActualUsage: actualUsage()}); err != nil {
		t.Fatalf("Settle settled reservation: %v", err)
	}
	recovered, err = db.svc.RecoverStaleHolds(ctx, time.Now().Add(48*time.Hour), 10)
	if err != nil {
		t.Fatalf("RecoverStaleHolds with settled reservation: %v", err)
	}
	if recovered.Reservations != 0 {
		t.Fatalf("recovery touched a settled reservation: %d", recovered.Reservations)
	}
}

func TestPostgresZeroPriceAndUnknownQuote(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := db.newUser(t)

	free, err := pricing.FreeQuote(pricing.Key{Provider: "openai", Model: "free-model"})
	if err != nil {
		t.Fatalf("FreeQuote: %v", err)
	}
	res := mustReserve(t, db.svc, user, "free-req", free, pricing.Usage{InputTokens: 1_000_000})
	checkMoney(t, "free hold", res.HoldMicros, 0)
	checkMoney(t, "free balance", mustBalance(t, db.svc, user).BalanceMicros, 0)

	settlement, err := db.svc.Settle(ctx, SettleRequest{ReservationID: res.ID, ActualUsage: pricing.Usage{InputTokens: 1_000_000}})
	if err != nil {
		t.Fatalf("Settle free reservation: %v", err)
	}
	checkMoney(t, "free charge", settlement.ChargedMicros, 0)
	checkMoney(t, "free released", settlement.ReleasedMicros, 0)
	if got := db.countEntries(t, user, "settle"); got != 1 {
		t.Fatalf("settle entries = %d, want 1", got)
	}

	// A zero-value Quote is not a free price: Reserve refuses it before any
	// row is written.
	user2 := db.newUser(t)
	if _, err := db.svc.Reserve(ctx, ReserveRequest{
		UserID:    user2,
		RequestID: "unpriced-req",
		Quote:     pricing.Quote{},
	}); !errors.Is(err, pricing.ErrInvalidKey) {
		t.Fatalf("zero quote error = %v, want pricing.ErrInvalidKey", err)
	}
	if got := db.countReservations(t, user2); got != 0 {
		t.Fatalf("reservations for zero quote = %d, want 0", got)
	}
	checkMoney(t, "zero quote balance", mustBalance(t, db.svc, user2).BalanceMicros, 0)

	// A table without the key fails lookup instead of returning zero.
	table, err := pricing.NewTable(map[pricing.Key]pricing.Price{})
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	if _, err := table.Lookup(pricing.Key{Provider: "openai", Model: "unknown"}); !errors.Is(err, pricing.ErrUnknownPrice) {
		t.Fatalf("unknown lookup error = %v, want pricing.ErrUnknownPrice", err)
	}
}

func TestPostgresSettlementMayOverdraw(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := db.newUser(t)
	mustCredit(t, db.svc, user, 1_000, "overdraw-topup")

	quote, err := pricing.NewQuote(
		pricing.Key{Provider: "openai", Model: "per-token"},
		pricing.Price{InputPerMTok: 1_000_000})
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	res := mustReserve(t, db.svc, user, "overdraw-req", quote, pricing.Usage{})
	checkMoney(t, "overdraw hold", res.HoldMicros, 0)

	// 1,500 tokens at 1 micro-CNY each costs 1,500 against a 1,000 balance.
	settlement, err := db.svc.Settle(ctx, SettleRequest{
		ReservationID: res.ID,
		ActualUsage:   pricing.Usage{InputTokens: 1_500},
	})
	if err != nil {
		t.Fatalf("Settle overdraw: %v", err)
	}
	checkMoney(t, "overdraw charge", settlement.ChargedMicros, 1_500)
	checkMoney(t, "overdraw balance", settlement.BalanceMicros, -500)

	balance := mustBalance(t, db.svc, user)
	checkMoney(t, "stored overdraw balance", balance.BalanceMicros, -500)
	checkMoney(t, "overdraw held", balance.HeldMicros, 0)
	checkMoney(t, "overdraw available", balance.AvailableMicros, -500)

	_, err = db.svc.Reserve(ctx, ReserveRequest{
		UserID:         user,
		RequestID:      "overdraw-next",
		Quote:          quote,
		EstimatedUsage: pricing.Usage{InputTokens: 100},
	})
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("Reserve with negative balance error = %v, want ErrInsufficientFunds", err)
	}
	var insufficient *InsufficientFundsError
	if !errors.As(err, &insufficient) {
		t.Fatalf("error %v is not *InsufficientFundsError", err)
	}
	checkMoney(t, "insufficient need", insufficient.Need, 100)
	checkMoney(t, "insufficient available", insufficient.Available, -500)
}

func TestPostgresInsufficientFundsChangesNothing(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := db.newUser(t)
	mustCredit(t, db.svc, user, 100, "small-topup")

	quote, err := pricing.NewQuote(
		pricing.Key{Provider: "openai", Model: "fixed-101"},
		pricing.Price{FixedPerCall: 101})
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if _, err := db.svc.Reserve(ctx, ReserveRequest{UserID: user, RequestID: "too-much", Quote: quote}); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("Reserve error = %v, want ErrInsufficientFunds", err)
	}
	balance := mustBalance(t, db.svc, user)
	checkMoney(t, "balance unchanged", balance.BalanceMicros, 100)
	checkMoney(t, "held unchanged", balance.HeldMicros, 0)
	if got := db.countReservations(t, user); got != 0 {
		t.Fatalf("reservations = %d, want 0", got)
	}
	if got := db.countEntries(t, user, "hold"); got != 0 {
		t.Fatalf("hold entries = %d, want 0", got)
	}
}

func TestPostgresReserveReplayTakesOneHold(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := db.newUser(t)
	mustCredit(t, db.svc, user, 100_000, "replay-topup")

	first := mustReserve(t, db.svc, user, "replay-req", testQuote(t), estimatedUsage())
	other, err := pricing.NewQuote(
		pricing.Key{Channel: "house", Provider: "openai", Model: "gpt-5"},
		pricing.Price{FixedPerCall: 99_999})
	if err != nil {
		t.Fatalf("second quote: %v", err)
	}
	second := db.newService(t)
	replay, err := second.Reserve(ctx, ReserveRequest{
		UserID:         user,
		RequestID:      "replay-req",
		Quote:          other,
		EstimatedUsage: pricing.Usage{InputTokens: 1_000_000},
	})
	if err != nil {
		t.Fatalf("Reserve replay: %v", err)
	}
	if !replay.Replayed {
		t.Fatal("reserve replay did not report Replayed")
	}
	if replay.ID != first.ID {
		t.Fatalf("replay reservation = %s, want %s", replay.ID, first.ID)
	}
	checkMoney(t, "replay hold", replay.HoldMicros, 15_850)
	checkMoney(t, "replay balance held", mustBalance(t, db.svc, user).HeldMicros, 15_850)
	if got := db.countEntries(t, user, "hold"); got != 1 {
		t.Fatalf("hold entries = %d, want 1", got)
	}
}
