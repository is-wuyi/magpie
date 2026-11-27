package channels

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/yetone/magpie/internal/provider"
)

func (s *Service) CreateUserGroup(ctx context.Context, userID uuid.UUID, g provider.Group) (provider.Group, error) {
	if strings.TrimSpace(g.Name) == "" || len(g.Members) == 0 {
		return provider.Group{}, errors.New("a routing group needs a name and at least one member")
	}
	if len(g.Match) != 0 || provider.PluginRouting(g.Routing) {
		return provider.Group{}, errors.New("pattern and plugin routing are not available in cloud groups")
	}
	if g.Classifier != "" {
		return provider.Group{}, errors.New("classifier side calls are not available in cloud groups")
	}
	id, err := publicID("g", userID, g.ID)
	if err != nil {
		return provider.Group{}, err
	}
	g.ID, g.OwnerUserID, g.HouseChannelID, g.Auto = id, userID.String(), "", false
	for _, member := range g.Members {
		if err := s.memberVisible(ctx, userID, member); err != nil {
			return provider.Group{}, err
		}
	}
	plain, err := json.Marshal(g)
	if err != nil {
		return provider.Group{}, err
	}
	sealed, err := s.cipher.Seal(plain)
	if err != nil {
		return provider.Group{}, err
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return provider.Group{}, err
	}
	defer tx.Rollback(ctx)
	groupID := uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO provider_groups (id, public_id, owner_user_id, name) VALUES ($1, $2, $3, $4)`, groupID, g.ID, userID, g.Name); err != nil {
		return provider.Group{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO provider_group_secrets (group_id, config_ciphertext) VALUES ($1, $2)`, groupID, sealed); err != nil {
		return provider.Group{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return provider.Group{}, err
	}
	provider.Changed()
	return g, nil
}

func (s *Service) memberVisible(ctx context.Context, userID uuid.UUID, member string) error {
	member = strings.TrimSpace(member)
	if member == "" {
		return errors.New("empty routing group member")
	}
	if strings.HasPrefix(member, provider.GroupPrefix) {
		var visible bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM provider_groups WHERE public_id = $1 AND owner_user_id = $2)`, strings.TrimPrefix(member, provider.GroupPrefix), userID).Scan(&visible); err != nil {
			return err
		}
		if !visible {
			return ErrForbidden
		}
		return nil
	}
	pid, _, ok := strings.Cut(member, "/")
	if !ok {
		return errors.New("invalid routing group member")
	}
	var owner *string
	var house *string
	err := s.db.QueryRow(ctx, `SELECT owner_user_id, house_channel_id FROM providers WHERE public_id = $1`, pid).Scan(&owner, &house)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if owner != nil && *owner == userID.String() {
		return nil
	}
	if house == nil {
		return ErrForbidden
	}
	channelID, _ := uuid.Parse(*house)
	granted, err := s.Granted(ctx, userID, channelID)
	if err != nil || !granted {
		return ErrForbidden
	}
	return nil
}

func (s *Service) LoadGroups(ctx context.Context) ([]provider.Group, error) {
	rows, err := s.db.Query(ctx, `SELECT g.public_id, g.owner_user_id, s.config_ciphertext
		FROM provider_groups g JOIN provider_group_secrets s ON s.group_id = g.id ORDER BY g.created_at, g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []provider.Group
	for rows.Next() {
		var publicID, owner string
		var sealed []byte
		if err := rows.Scan(&publicID, &owner, &sealed); err != nil {
			return nil, err
		}
		plain, err := s.cipher.Open(sealed)
		if err != nil {
			return nil, err
		}
		var g provider.Group
		if err := json.Unmarshal(plain, &g); err != nil {
			return nil, err
		}
		g.ID, g.OwnerUserID, g.HouseChannelID, g.Auto = publicID, owner, "", false
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Service) LoadGroupCatalog() ([]provider.Group, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return s.LoadGroups(ctx)
}

func (s *Service) ListUserGroups(ctx context.Context, userID uuid.UUID) ([]provider.Group, error) {
	gs, err := s.LoadGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := []provider.Group{}
	for _, g := range gs {
		if g.OwnerUserID == userID.String() {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *Service) DeleteUserGroup(ctx context.Context, userID uuid.UUID, publicID string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM provider_groups WHERE public_id = $1 AND owner_user_id = $2`, publicID, userID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err == nil {
		provider.Changed()
	}
	return err
}
