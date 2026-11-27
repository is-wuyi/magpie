// Package channels persists tenant-owned providers, house channels and their
// explicit user grants. Provider configurations are encrypted as one blob.
package channels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yetone/magpie/internal/provider"
)

var (
	ErrNotFound  = errors.New("cloud channel not found")
	ErrForbidden = errors.New("cloud channel belongs to another user")
)

type Service struct {
	db     *pgxpool.Pool
	cipher *Cipher
	now    func() time.Time
}

func New(db *pgxpool.Pool, cipher *Cipher) *Service {
	return &Service{db: db, cipher: cipher, now: time.Now}
}

type HouseChannel struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type KeyView struct {
	Name   string `json:"name"`
	Masked string `json:"masked"`
	Off    bool   `json:"off"`
}

// ProviderView is safe to return to a tenant. It deliberately contains no
// credentials, arbitrary headers, credential paths or provider extras.
type ProviderView struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	OwnerUserID    string    `json:"ownerUserId,omitempty"`
	HouseChannelID string    `json:"houseChannelId,omitempty"`
	Chat           string    `json:"chat,omitempty"`
	Responses      string    `json:"responses,omitempty"`
	Anthropic      string    `json:"anthropic,omitempty"`
	Gemini         string    `json:"gemini,omitempty"`
	Models         []string  `json:"models"`
	Off            bool      `json:"off"`
	KeySet         bool      `json:"keySet"`
	Keys           []KeyView `json:"keys"`
}

func viewOf(p provider.Provider) ProviderView {
	v := ProviderView{ID: p.ID, Name: p.Name, OwnerUserID: p.OwnerUserID, HouseChannelID: p.HouseChannelID,
		Chat: p.Chat, Responses: p.Responses, Anthropic: p.Anthropic, Gemini: p.Gemini, Models: p.Models, Off: p.Off,
		KeySet: p.Key != "" || len(p.Keys) > 0}
	if p.Key != "" {
		v.Keys = append(v.Keys, KeyView{Name: p.KeyName, Masked: provider.Mask(p.Key)})
	}
	for _, k := range p.Keys {
		v.Keys = append(v.Keys, KeyView{Name: k.Name, Masked: provider.Mask(k.Key), Off: k.Off})
	}
	return v
}

func validateProvider(p provider.Provider) error {
	if p.Account != nil {
		return errors.New("subscription and OAuth providers are not available in cloud mode")
	}
	if p.Name = strings.TrimSpace(p.Name); p.Name == "" {
		return errors.New("provider name is required")
	}
	if p.Chat == "" && p.Responses == "" && p.Anthropic == "" && p.Gemini == "" {
		return errors.New("a keyed provider needs at least one API endpoint")
	}
	if p.Key == "" {
		has := false
		for _, k := range p.Keys {
			has = has || strings.TrimSpace(k.Key) != ""
		}
		if !has {
			return errors.New("cloud providers require an API key; local credential discovery is disabled")
		}
	}
	if p.Vertex != nil && p.Vertex.Credentials != "" {
		return errors.New("local Google credential files are not available; use a Vertex API key")
	}
	if provider.PluginRouting(p.Routing) {
		return errors.New("plugin routing is not available in cloud mode")
	}
	return nil
}

func publicID(prefix string, owner uuid.UUID, id string) (string, error) {
	slug := provider.Slug(id)
	if slug == "" {
		return "", errors.New("provider id is required")
	}
	return fmt.Sprintf("%s-%s-%s", prefix, strings.ReplaceAll(owner.String(), "-", "")[:12], slug), nil
}

func (s *Service) CreateUserProvider(ctx context.Context, userID uuid.UUID, p provider.Provider) (ProviderView, error) {
	if err := validateProvider(p); err != nil {
		return ProviderView{}, err
	}
	id, err := publicID("u", userID, p.ID)
	if err != nil {
		return ProviderView{}, err
	}
	p.ID, p.OwnerUserID, p.HouseChannelID = id, userID.String(), ""
	if err := s.insert(ctx, uuid.New(), p); err != nil {
		return ProviderView{}, err
	}
	provider.Changed()
	return viewOf(p), nil
}

func (s *Service) CreateHouseProvider(ctx context.Context, channelID uuid.UUID, p provider.Provider) (ProviderView, error) {
	if err := validateProvider(p); err != nil {
		return ProviderView{}, err
	}
	id, err := publicID("h", channelID, p.ID)
	if err != nil {
		return ProviderView{}, err
	}
	p.ID, p.OwnerUserID, p.HouseChannelID = id, "", channelID.String()
	if err := s.insert(ctx, uuid.New(), p); err != nil {
		return ProviderView{}, err
	}
	provider.Changed()
	return viewOf(p), nil
}

func (s *Service) insert(ctx context.Context, id uuid.UUID, p provider.Provider) error {
	plain, err := json.Marshal(p)
	if err != nil {
		return err
	}
	sealed, err := s.cipher.Seal(plain)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO providers (id, public_id, owner_user_id, house_channel_id, name)
		VALUES ($1, $2, $3, $4, $5)`, id, p.ID, nullable(p.OwnerUserID), nullable(p.HouseChannelID), p.Name)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO provider_secrets (provider_id, config_ciphertext) VALUES ($1, $2)`, id, sealed)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// LoadProviders feeds provider.RegisterCloudLoader. A read or decryption
// error fails the entire catalog; it never silently drops authorization data.
func (s *Service) LoadProviders(ctx context.Context) ([]provider.Provider, error) {
	rows, err := s.db.Query(ctx, `SELECT p.public_id, p.owner_user_id, p.house_channel_id, s.config_ciphertext
		FROM providers p JOIN provider_secrets s ON s.provider_id = p.id ORDER BY p.created_at, p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []provider.Provider
	for rows.Next() {
		var publicID string
		var owner, house *string
		var sealed []byte
		if err := rows.Scan(&publicID, &owner, &house, &sealed); err != nil {
			return nil, err
		}
		plain, err := s.cipher.Open(sealed)
		if err != nil {
			return nil, fmt.Errorf("decrypt provider %s: %w", publicID, err)
		}
		var p provider.Provider
		if err := json.Unmarshal(plain, &p); err != nil {
			return nil, fmt.Errorf("decode provider %s: %w", publicID, err)
		}
		p.ID = publicID
		p.OwnerUserID, p.HouseChannelID = "", ""
		if owner != nil {
			p.OwnerUserID = *owner
		}
		if house != nil {
			p.HouseChannelID = *house
		}
		if err := validateProvider(p); err != nil {
			return nil, fmt.Errorf("provider %s: %w", publicID, err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) ListUserProviders(ctx context.Context, userID uuid.UUID) ([]ProviderView, error) {
	ps, err := s.LoadProviders(ctx)
	if err != nil {
		return nil, err
	}
	out := []ProviderView{}
	for _, p := range ps {
		if p.OwnerUserID == userID.String() {
			out = append(out, viewOf(p))
		}
	}
	return out, nil
}

func (s *Service) ListHouseProviders(ctx context.Context, channelID uuid.UUID) ([]ProviderView, error) {
	ps, err := s.LoadProviders(ctx)
	if err != nil {
		return nil, err
	}
	out := []ProviderView{}
	for _, p := range ps {
		if p.HouseChannelID == channelID.String() {
			out = append(out, viewOf(p))
		}
	}
	return out, nil
}

func (s *Service) DeleteUserProvider(ctx context.Context, userID uuid.UUID, publicID string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM providers WHERE public_id = $1 AND owner_user_id = $2`, publicID, userID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err == nil {
		provider.Changed()
	}
	return err
}

func (s *Service) DeleteHouseProvider(ctx context.Context, channelID uuid.UUID, publicID string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM providers WHERE public_id = $1 AND house_channel_id = $2`, publicID, channelID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err == nil {
		provider.Changed()
	}
	return err
}

func (s *Service) CreateHouseChannel(ctx context.Context, name, description string) (HouseChannel, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return HouseChannel{}, errors.New("house channel name is required")
	}
	c := HouseChannel{ID: uuid.New(), Name: name, Description: strings.TrimSpace(description), Status: "draft", CreatedAt: s.now().UTC(), UpdatedAt: s.now().UTC()}
	_, err := s.db.Exec(ctx, `INSERT INTO house_channels (id, name, description, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, c.ID, c.Name, c.Description, c.Status, c.CreatedAt, c.UpdatedAt)
	return c, err
}

func (s *Service) SetHouseStatus(ctx context.Context, id uuid.UUID, status string) error {
	if status != "draft" && status != "published" && status != "offline" {
		return errors.New("invalid house channel status")
	}
	tag, err := s.db.Exec(ctx, `UPDATE house_channels SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Service) ListHouseChannels(ctx context.Context, includeUnpublished bool) ([]HouseChannel, error) {
	where := ""
	if !includeUnpublished {
		where = "WHERE status = 'published'"
	}
	rows, err := s.db.Query(ctx, `SELECT id, name, description, status, created_at, updated_at FROM house_channels `+where+` ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HouseChannel{}
	for rows.Next() {
		var c HouseChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Description, &c.Status, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Service) Grant(ctx context.Context, userID, channelID uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `INSERT INTO channel_grants (user_id, channel_id)
		SELECT $1, id FROM house_channels WHERE id = $2 AND status = 'published' ON CONFLICT DO NOTHING`, userID, channelID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var granted bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM channel_grants WHERE user_id = $1 AND channel_id = $2)`, userID, channelID).Scan(&granted); err != nil {
			return err
		}
		if granted {
			return nil
		}
		return ErrNotFound
	}
	return nil
}

func (s *Service) Granted(ctx context.Context, userID, channelID uuid.UUID) (bool, error) {
	var granted bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM channel_grants g JOIN house_channels h ON h.id = g.channel_id
		WHERE g.user_id = $1 AND g.channel_id = $2 AND h.status = 'published')`, userID, channelID).Scan(&granted)
	return granted, err
}

func (s *Service) Revoke(ctx context.Context, userID, channelID uuid.UUID) error {
	_, err := s.db.Exec(ctx, `DELETE FROM channel_grants WHERE user_id = $1 AND channel_id = $2`, userID, channelID)
	return err
}

// Load returns the currently authorized provider list for the global catalog.
// Authorization itself is applied to each request by the gateway identity.
func (s *Service) Load() ([]provider.Provider, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return s.LoadProviders(ctx)
}
