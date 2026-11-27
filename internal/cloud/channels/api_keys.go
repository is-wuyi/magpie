package channels

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
)

var ErrInvalidAPIKey = errors.New("API key is disabled, expired or unknown")

type APIKeyView struct {
	ID             uuid.UUID  `json:"id"`
	Name           string     `json:"name"`
	Masked         string     `json:"masked"`
	Models         []string   `json:"models"`
	HouseChannels  []string   `json:"houseChannels"`
	Disabled       bool       `json:"disabled"`
	ExpiresAt      *time.Time `json:"expiresAt,omitempty"`
	MaxConcurrency int        `json:"maxConcurrency"`
	CreatedAt      time.Time  `json:"createdAt"`
	LastUsedAt     *time.Time `json:"lastUsedAt,omitempty"`
}

type CreatedAPIKey struct {
	APIKeyView
	Secret string `json:"secret"`
}

type APIKeys struct{ db *pgxpool.Pool }

func NewAPIKeys(db *pgxpool.Pool) *APIKeys { return &APIKeys{db: db} }

func randomSecret() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return access.Prefix + hex.EncodeToString(b), nil
}

func secretHash(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

func (s *APIKeys) Create(ctx context.Context, userID uuid.UUID, name string, models, houseChannels []string, expiresAt *time.Time, maxConcurrency int) (CreatedAPIKey, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return CreatedAPIKey{}, errors.New("API key name must be between 1 and 100 characters")
	}
	models, err := access.CleanModels(models)
	if err != nil {
		return CreatedAPIKey{}, err
	}
	if maxConcurrency < 0 || maxConcurrency > access.MaxKeyConcurrency {
		return CreatedAPIKey{}, errors.New("invalid API key concurrency limit")
	}
	secret, err := randomSecret()
	if err != nil {
		return CreatedAPIKey{}, err
	}
	view := APIKeyView{ID: uuid.New(), Name: name, Masked: provider.Mask(secret), Models: models, MaxConcurrency: maxConcurrency, ExpiresAt: expiresAt, CreatedAt: time.Now().UTC()}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return CreatedAPIKey{}, err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO api_keys (id, user_id, name, secret_hash, masked, models, expires_at, max_concurrency)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8)`, view.ID, userID, name, secretHash(secret), view.Masked, mustJSON(models), expiresAt, maxConcurrency)
	if err != nil {
		return CreatedAPIKey{}, err
	}
	for _, raw := range houseChannels {
		channelID, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			return CreatedAPIKey{}, errors.New("invalid house channel id")
		}
		tag, err := tx.Exec(ctx, `INSERT INTO api_key_channels (key_id, channel_id)
			SELECT $1, g.channel_id FROM channel_grants g JOIN house_channels h ON h.id = g.channel_id
			WHERE g.user_id = $2 AND g.channel_id = $3 AND h.status = 'published'`, view.ID, userID, channelID)
		if err != nil {
			return CreatedAPIKey{}, err
		}
		if tag.RowsAffected() != 1 {
			return CreatedAPIKey{}, ErrForbidden
		}
		view.HouseChannels = append(view.HouseChannels, channelID.String())
	}
	if err := tx.Commit(ctx); err != nil {
		return CreatedAPIKey{}, err
	}
	return CreatedAPIKey{APIKeyView: view, Secret: secret}, nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (s *APIKeys) Authenticate(secret string) (access.Identity, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return s.authenticate(ctx, secretHash(secret))
}

func (s *APIKeys) authenticate(ctx context.Context, hash []byte) (access.Identity, error) {
	var id, userID uuid.UUID
	var name string
	var modelsJSON []byte
	var expires *time.Time
	var maxConcurrency int
	err := s.db.QueryRow(ctx, `SELECT k.id, k.user_id, k.name, k.models, k.expires_at, k.max_concurrency
		FROM api_keys k JOIN users u ON u.id = k.user_id
		WHERE k.secret_hash = $1 AND NOT k.disabled AND u.status = 'active'
		AND (k.expires_at IS NULL OR k.expires_at > now())`, hash).Scan(&id, &userID, &name, &modelsJSON, &expires, &maxConcurrency)
	if errors.Is(err, pgx.ErrNoRows) {
		return access.Identity{}, ErrInvalidAPIKey
	}
	if err != nil {
		return access.Identity{}, err
	}
	var models []string
	if err := json.Unmarshal(modelsJSON, &models); err != nil {
		return access.Identity{}, err
	}
	rows, err := s.db.Query(ctx, `SELECT g.channel_id::text FROM channel_grants g
		JOIN house_channels h ON h.id = g.channel_id
		WHERE g.user_id = $1 AND h.status = 'published'
		AND (NOT EXISTS (SELECT 1 FROM api_key_channels b WHERE b.key_id = $2)
		OR EXISTS (SELECT 1 FROM api_key_channels b WHERE b.key_id = $2 AND b.channel_id = g.channel_id))`, userID, id)
	if err != nil {
		return access.Identity{}, err
	}
	defer rows.Close()
	var houseChannels []string
	for rows.Next() {
		var channel string
		if err := rows.Scan(&channel); err != nil {
			return access.Identity{}, err
		}
		houseChannels = append(houseChannels, channel)
	}
	if err := rows.Err(); err != nil {
		return access.Identity{}, err
	}
	var expiresAt time.Time
	if expires != nil {
		expiresAt = *expires
	}
	_, _ = s.db.Exec(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1
		AND (last_used_at IS NULL OR last_used_at < now() - interval '5 minutes')`, id)
	return access.Identity{Cloud: true, UserID: userID.String(), KeyID: id.String(), KeyName: name,
		Models: models, HouseChannels: houseChannels, Expires: expiresAt, MaxConcurrency: maxConcurrency}, nil
}

func (s *APIKeys) Current(who access.Identity, sum [32]byte) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := s.authenticate(ctx, sum[:])
	return err == nil && got.KeyID == who.KeyID && got.UserID == who.UserID
}

func (s *APIKeys) Revoke(ctx context.Context, userID, keyID uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `UPDATE api_keys SET disabled = true WHERE id = $1 AND user_id = $2`, keyID, userID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *APIKeys) List(ctx context.Context, userID uuid.UUID) ([]APIKeyView, error) {
	rows, err := s.db.Query(ctx, `SELECT id, name, masked, models, disabled, expires_at, max_concurrency, created_at, last_used_at
		FROM api_keys WHERE user_id = $1 ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKeyView{}
	for rows.Next() {
		var v APIKeyView
		var modelsJSON []byte
		if err := rows.Scan(&v.ID, &v.Name, &v.Masked, &modelsJSON, &v.Disabled, &v.ExpiresAt, &v.MaxConcurrency, &v.CreatedAt, &v.LastUsedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(modelsJSON, &v.Models); err != nil {
			return nil, err
		}
		channels, err := s.db.Query(ctx, `SELECT channel_id::text FROM api_key_channels WHERE key_id = $1 ORDER BY channel_id`, v.ID)
		if err != nil {
			return nil, err
		}
		for channels.Next() {
			var id string
			if err := channels.Scan(&id); err != nil {
				channels.Close()
				return nil, err
			}
			v.HouseChannels = append(v.HouseChannels, id)
		}
		if err := channels.Err(); err != nil {
			channels.Close()
			return nil, err
		}
		channels.Close()
		out = append(out, v)
	}
	return out, rows.Err()
}
