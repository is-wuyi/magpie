package channels

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/yetone/magpie/internal/cloud/pricing"
)

type AdminUser struct {
	ID            uuid.UUID      `json:"id"`
	Email         string         `json:"email"`
	Role          string         `json:"role"`
	Status        string         `json:"status"`
	CreatedAt     time.Time      `json:"createdAt"`
	BalanceMicros pricing.Micros `json:"balanceMicros"`
	HeldMicros    pricing.Micros `json:"heldMicros"`
}

type AuditEntry struct {
	ID         uuid.UUID       `json:"id"`
	ActorID    *uuid.UUID      `json:"actorUserId,omitempty"`
	Action     string          `json:"action"`
	TargetType string          `json:"targetType"`
	TargetID   string          `json:"targetId"`
	Details    json.RawMessage `json:"details"`
	CreatedAt  time.Time       `json:"createdAt"`
}

type AdminOrder struct {
	MerchantOrderNo string     `json:"merchantOrderNo"`
	UserID          uuid.UUID  `json:"userId"`
	Provider        string     `json:"provider"`
	AmountMicros    int64      `json:"amountMicros"`
	Status          string     `json:"status"`
	CreditState     string     `json:"creditState"`
	CreatedAt       time.Time  `json:"createdAt"`
	PaidAt          *time.Time `json:"paidAt,omitempty"`
}

func (s *Service) ListUsers(ctx context.Context) ([]AdminUser, error) {
	rows, err := s.db.Query(ctx, `SELECT u.id, u.email, u.role, u.status, u.created_at,
		COALESCE(w.balance_micros, 0), COALESCE(w.held_micros, 0)
		FROM users u LEFT JOIN wallets w ON w.user_id = u.id ORDER BY u.created_at, u.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminUser{}
	for rows.Next() {
		var u AdminUser
		if err := rows.Scan(&u.ID, &u.Email, &u.Role, &u.Status, &u.CreatedAt, &u.BalanceMicros, &u.HeldMicros); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Service) SetUserStatus(ctx context.Context, userID uuid.UUID, status string) error {
	if status != "active" && status != "disabled" {
		return errors.New("invalid user status")
	}
	tag, err := s.db.Exec(ctx, `UPDATE users SET status = $2, auth_version = auth_version + 1, updated_at = now() WHERE id = $1`, userID, status)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Service) Audit(ctx context.Context, actorID uuid.UUID, action, targetType, targetID string, details any, ip string) error {
	id := uuid.New()
	var actor any
	if actorID != uuid.Nil {
		actor = actorID
	}
	var address any
	if ip != "" {
		address = ip
	}
	_, err := s.db.Exec(ctx, `INSERT INTO audit_logs (id, actor_user_id, action, target_type, target_id, details, request_ip)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)`, id, actor, action, targetType, targetID, mustJSON(details), address)
	return err
}

func (s *Service) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.Query(ctx, `SELECT id, actor_user_id, action, target_type, target_id, details, created_at
		FROM audit_logs ORDER BY created_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.ActorID, &e.Action, &e.TargetType, &e.TargetID, &e.Details, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Service) ListOrders(ctx context.Context, limit int) ([]AdminOrder, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.Query(ctx, `SELECT merchant_order_no, user_id, provider, amount_micro_cny, status, credit_state, created_at, paid_at
		FROM payment_orders ORDER BY created_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminOrder{}
	for rows.Next() {
		var o AdminOrder
		if err := rows.Scan(&o.MerchantOrderNo, &o.UserID, &o.Provider, &o.AmountMicros, &o.Status, &o.CreditState, &o.CreatedAt, &o.PaidAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
