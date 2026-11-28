package auth

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/yetone/magpie/internal/cloud/store"
)

// fakeRepo is an in-memory Repository. Its rules mirror the store's for the
// cases the service tests exercise; the SQL itself is covered by the store's
// opt-in PostgreSQL tests.
type fakeRepo struct {
	mu sync.Mutex

	codes    map[uuid.UUID]store.EmailCode
	users    map[uuid.UUID]store.User
	byEmail  map[string]uuid.UUID
	sessions map[string]store.Session

	policy      store.AuthPolicy
	credentials map[string]store.UserCredentials
	attempts    []passwordAttempt

	lastIssue  store.IssueEmailCodeParams
	lastEnsure store.EnsureVerifiedUserParams
	deleted    []uuid.UUID
	touchCalls []time.Time

	issueErr      error
	latestErr     error
	recordErr     error
	consumeErr    error
	deleteErr     error
	countsErr     error
	ensureErr     error
	createErr     error
	sessionErr    error
	touchErr      error
	revokeErr     error
	revokeAllErr  error
	userByIDErr   error
	counts        *store.FailureCounts
	issueCalls    int
	latestCalls   int
	countsCalls   int
	consumeCalls  int
	createCalls   int
	revokeAllCall int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		codes:    make(map[uuid.UUID]store.EmailCode),
		users:    make(map[uuid.UUID]store.User),
		byEmail:  make(map[string]uuid.UUID),
		sessions: make(map[string]store.Session),
		policy: store.AuthPolicy{
			RegistrationEnabled: true, PasswordLoginEnabled: true, CodeLoginEnabled: true, PasswordMinLength: 12, Version: 1,
		},
		credentials: make(map[string]store.UserCredentials),
	}
}

func (f *fakeRepo) IssueEmailCode(ctx context.Context, p store.IssueEmailCodeParams) (store.EmailCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issueCalls++
	f.lastIssue = p
	if f.issueErr != nil {
		return store.EmailCode{}, f.issueErr
	}
	for id, old := range f.codes {
		if old.EmailNormalized == p.EmailNormalized && old.Purpose == p.Purpose && old.ConsumedAt == nil {
			at := p.Now
			old.ConsumedAt = &at
			f.codes[id] = old
		}
	}
	c := store.EmailCode{
		ID:              uuid.New(),
		EmailNormalized: p.EmailNormalized,
		Purpose:         p.Purpose,
		CodeHash:        append([]byte(nil), p.CodeHash...),
		HashVersion:     p.HashVersion,
		Attempts:        0,
		MaxAttempts:     p.MaxAttempts,
		CreatedAt:       p.Now,
		ExpiresAt:       p.ExpiresAt,
		RequestIP:       p.RequestIP,
	}
	f.codes[c.ID] = c
	return c, nil
}

func (f *fakeRepo) LatestActiveEmailCode(ctx context.Context, emailNormalized, purpose string, now time.Time) (store.EmailCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latestCalls++
	if f.latestErr != nil {
		return store.EmailCode{}, f.latestErr
	}
	var best *store.EmailCode
	for _, c := range f.codes {
		if c.EmailNormalized != emailNormalized || c.Purpose != purpose {
			continue
		}
		if c.ConsumedAt != nil || c.Attempts >= c.MaxAttempts || !c.ExpiresAt.After(now) {
			continue
		}
		if best == nil || c.CreatedAt.After(best.CreatedAt) {
			cp := c
			best = &cp
		}
	}
	if best == nil {
		return store.EmailCode{}, store.ErrNotFound
	}
	return *best, nil
}

func (f *fakeRepo) RecordEmailCodeFailure(ctx context.Context, id uuid.UUID, now time.Time) (store.EmailCodeAttempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return store.EmailCodeAttempt{}, f.recordErr
	}
	c, ok := f.codes[id]
	if !ok || c.ConsumedAt != nil || c.Attempts >= c.MaxAttempts {
		return store.EmailCodeAttempt{}, store.ErrCodeUnavailable
	}
	c.Attempts++
	if c.Attempts >= c.MaxAttempts {
		at := now
		c.ConsumedAt = &at
	}
	f.codes[id] = c
	return store.EmailCodeAttempt{Attempts: c.Attempts, MaxAttempts: c.MaxAttempts, Exhausted: c.Attempts >= c.MaxAttempts}, nil
}

func (f *fakeRepo) ConsumeEmailCode(ctx context.Context, id uuid.UUID, codeHash []byte, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.consumeCalls++
	if f.consumeErr != nil {
		return f.consumeErr
	}
	c, ok := f.codes[id]
	if !ok || c.ConsumedAt != nil || c.Attempts >= c.MaxAttempts || !c.ExpiresAt.After(now) || !bytes.Equal(c.CodeHash, codeHash) {
		return store.ErrCodeUnavailable
	}
	at := now
	c.ConsumedAt = &at
	f.codes[id] = c
	return nil
}

func (f *fakeRepo) DeleteEmailCode(ctx context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if c, ok := f.codes[id]; ok {
		at := time.Now()
		c.ConsumedAt = &at
		f.codes[id] = c
	}
	return nil
}

func (f *fakeRepo) EmailCodeFailureCounts(ctx context.Context, emailNormalized, requestIP string, since time.Time) (store.FailureCounts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.countsCalls++
	if f.countsErr != nil {
		return store.FailureCounts{}, f.countsErr
	}
	if f.counts != nil {
		return *f.counts, nil
	}
	var out store.FailureCounts
	for _, c := range f.codes {
		if !c.CreatedAt.After(since) {
			continue
		}
		if c.EmailNormalized == emailNormalized {
			out.ByEmail += int(c.Attempts)
		}
		if requestIP != "" && c.RequestIP == requestIP {
			out.ByIP += int(c.Attempts)
		}
	}
	return out, nil
}

func (f *fakeRepo) EnsureVerifiedUser(ctx context.Context, p store.EnsureVerifiedUserParams) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastEnsure = p
	if f.ensureErr != nil {
		return store.User{}, f.ensureErr
	}
	if id, ok := f.byEmail[p.EmailNormalized]; ok {
		u := f.users[id]
		if u.Status != store.StatusActive {
			return store.User{}, store.ErrUserDisabled
		}
		u.Email = p.Email
		u.UpdatedAt = p.VerifiedAt
		if p.PromoteAdmin {
			u.Role = store.RoleAdmin
		}
		if u.EmailVerifiedAt == nil {
			at := p.VerifiedAt
			u.EmailVerifiedAt = &at
		}
		f.users[u.ID] = u
		return u, nil
	}
	role := store.RoleUser
	if p.PromoteAdmin {
		role = store.RoleAdmin
	}
	at := p.VerifiedAt
	u := store.User{
		ID:              uuid.New(),
		Email:           p.Email,
		EmailNormalized: p.EmailNormalized,
		Role:            role,
		Status:          store.StatusActive,
		AuthVersion:     1,
		EmailVerifiedAt: &at,
		CreatedAt:       p.VerifiedAt,
		UpdatedAt:       p.VerifiedAt,
	}
	f.users[u.ID] = u
	f.byEmail[u.EmailNormalized] = u.ID
	return u, nil
}

func (f *fakeRepo) UserByID(ctx context.Context, id uuid.UUID) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.userByIDErr != nil {
		return store.User{}, f.userByIDErr
	}
	u, ok := f.users[id]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}

func (f *fakeRepo) CreateSession(ctx context.Context, p store.CreateSessionParams) (store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	if f.createErr != nil {
		return store.Session{}, f.createErr
	}
	u, ok := f.users[p.UserID]
	if !ok || u.Status != store.StatusActive || u.EmailVerifiedAt == nil || u.AuthVersion != p.AuthVersion {
		return store.Session{}, store.ErrSessionStale
	}
	s := store.Session{
		ID:          uuid.New(),
		UserID:      p.UserID,
		TokenHash:   append([]byte(nil), p.TokenHash...),
		AuthVersion: p.AuthVersion,
		CreatedAt:   p.Now,
		LastSeenAt:  p.Now,
		ExpiresAt:   p.ExpiresAt,
		UserAgent:   p.UserAgent,
		RequestIP:   p.RequestIP,
	}
	f.sessions[hex.EncodeToString(p.TokenHash)] = s
	return s, nil
}

func (f *fakeRepo) SessionByTokenHash(ctx context.Context, tokenHash []byte, now time.Time) (store.Session, store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sessionErr != nil {
		return store.Session{}, store.User{}, f.sessionErr
	}
	s, ok := f.sessions[hex.EncodeToString(tokenHash)]
	if !ok || s.RevokedAt != nil || !s.ExpiresAt.After(now) {
		return store.Session{}, store.User{}, store.ErrNotFound
	}
	u, ok := f.users[s.UserID]
	if !ok || u.Status != store.StatusActive || u.AuthVersion != s.AuthVersion || u.EmailVerifiedAt == nil {
		return store.Session{}, store.User{}, store.ErrNotFound
	}
	return s, u, nil
}

func (f *fakeRepo) TouchSession(ctx context.Context, id uuid.UUID, now time.Time, minInterval time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.touchErr != nil {
		return f.touchErr
	}
	f.touchCalls = append(f.touchCalls, now)
	for key, s := range f.sessions {
		if s.ID == id && s.RevokedAt == nil && s.ExpiresAt.After(now) && !s.LastSeenAt.Add(minInterval).After(now) {
			s.LastSeenAt = now
			f.sessions[key] = s
		}
	}
	return nil
}

func (f *fakeRepo) RevokeSessionByTokenHash(ctx context.Context, tokenHash []byte, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revokeErr != nil {
		return f.revokeErr
	}
	key := hex.EncodeToString(tokenHash)
	s, ok := f.sessions[key]
	if !ok || s.RevokedAt != nil {
		return store.ErrNotFound
	}
	at := now
	s.RevokedAt = &at
	f.sessions[key] = s
	return nil
}

func (f *fakeRepo) RevokeUserSessions(ctx context.Context, userID uuid.UUID, now time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokeAllCall++
	if f.revokeAllErr != nil {
		return 0, f.revokeAllErr
	}
	u, ok := f.users[userID]
	if !ok {
		return 0, store.ErrNotFound
	}
	u.AuthVersion++
	u.UpdatedAt = now
	f.users[userID] = u
	for key, s := range f.sessions {
		if s.UserID == userID && s.RevokedAt == nil {
			at := now
			s.RevokedAt = &at
			f.sessions[key] = s
		}
	}
	return u.AuthVersion, nil
}

type passwordAttempt struct {
	email string
	ip    string
	at    time.Time
}

func (f *fakeRepo) AuthPolicy(context.Context) (store.AuthPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.policy, nil
}

func (f *fakeRepo) CreateRegisteredUser(_ context.Context, p store.CreateRegisteredUserParams) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.byEmail[p.EmailNormalized]; exists {
		return store.User{}, store.ErrEmailTaken
	}
	c, ok := f.codes[p.CodeID]
	if !ok || c.Purpose != PurposeRegister || !bytes.Equal(c.CodeHash, p.CodeHash) {
		return store.User{}, store.ErrCodeUnavailable
	}
	if err := f.consumeCodeLocked(p.CodeID, p.CodeHash, p.VerifiedAt); err != nil {
		return store.User{}, err
	}
	role := store.RoleUser
	if p.PromoteAdmin {
		role = store.RoleAdmin
	}
	at := p.VerifiedAt
	u := store.User{ID: uuid.New(), Email: p.Email, EmailNormalized: p.EmailNormalized, Role: role, Status: store.StatusActive,
		AuthVersion: 1, EmailVerifiedAt: &at, CreatedAt: at, UpdatedAt: at}
	f.users[u.ID] = u
	f.byEmail[u.EmailNormalized] = u.ID
	f.credentials[u.EmailNormalized] = store.UserCredentials{User: u, PasswordHash: p.PasswordHash, PasswordHashVersion: p.PasswordHashVersion}
	return u, nil
}

func (f *fakeRepo) CredentialsByEmail(_ context.Context, emailNormalized string) (store.UserCredentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.credentials[emailNormalized]; ok {
		return c, nil
	}
	id, ok := f.byEmail[emailNormalized]
	if !ok {
		return store.UserCredentials{}, store.ErrNotFound
	}
	return store.UserCredentials{User: f.users[id]}, nil
}

func (f *fakeRepo) ResetPassword(_ context.Context, p store.ResetPasswordParams) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.byEmail[p.EmailNormalized]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	c, ok := f.codes[p.CodeID]
	if !ok || c.Purpose != PurposeReset || !bytes.Equal(c.CodeHash, p.CodeHash) {
		return store.User{}, store.ErrCodeUnavailable
	}
	if err := f.consumeCodeLocked(p.CodeID, p.CodeHash, p.Now); err != nil {
		return store.User{}, err
	}
	u := f.users[id]
	u.AuthVersion++
	u.UpdatedAt = p.Now
	f.users[id] = u
	credentials := f.credentials[p.EmailNormalized]
	credentials.User = u
	credentials.PasswordHash = p.PasswordHash
	credentials.PasswordHashVersion = p.PasswordHashVersion
	f.credentials[p.EmailNormalized] = credentials
	for key, session := range f.sessions {
		if session.UserID == id && session.RevokedAt == nil {
			at := p.Now
			session.RevokedAt = &at
			f.sessions[key] = session
		}
	}
	return u, nil
}

func (f *fakeRepo) consumeCodeLocked(id uuid.UUID, hash []byte, now time.Time) error {
	c, ok := f.codes[id]
	if !ok || c.ConsumedAt != nil || c.Attempts >= c.MaxAttempts || !c.ExpiresAt.After(now) || !bytes.Equal(c.CodeHash, hash) {
		return store.ErrCodeUnavailable
	}
	at := now
	c.ConsumedAt = &at
	f.codes[id] = c
	return nil
}

func (f *fakeRepo) RecordAuthAttempt(_ context.Context, emailNormalized, requestIP string, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, passwordAttempt{email: emailNormalized, ip: requestIP, at: now})
	return nil
}

func (f *fakeRepo) AuthFailureCounts(_ context.Context, emailNormalized, requestIP string, since time.Time) (store.FailureCounts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out store.FailureCounts
	for _, attempt := range f.attempts {
		if !attempt.at.After(since) {
			continue
		}
		if attempt.email == emailNormalized {
			out.ByEmail++
		}
		if requestIP != "" && attempt.ip == requestIP {
			out.ByIP++
		}
	}
	return out, nil
}

func (f *fakeRepo) ClearAuthAttempts(_ context.Context, emailNormalized string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.attempts[:0]
	for _, attempt := range f.attempts {
		if attempt.email != emailNormalized {
			kept = append(kept, attempt)
		}
	}
	f.attempts = kept
	return nil
}

// onlyCode returns the newest code the repo holds.
func (f *fakeRepo) onlyCode() store.EmailCode {
	f.mu.Lock()
	defer f.mu.Unlock()
	var best *store.EmailCode
	for _, c := range f.codes {
		if best == nil || c.CreatedAt.After(best.CreatedAt) {
			cp := c
			best = &cp
		}
	}
	if best == nil {
		panic("fakeRepo has no code")
	}
	return *best
}

func (f *fakeRepo) userCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.users)
}

func (f *fakeRepo) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

type fakeMailer struct {
	mu   sync.Mutex
	msgs []LoginCodeMessage
	err  error
}

func (m *fakeMailer) SendLoginCode(ctx context.Context, msg LoginCodeMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, msg)
	return m.err
}

func (m *fakeMailer) last() LoginCodeMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.msgs) == 0 {
		panic("fakeMailer sent nothing")
	}
	return m.msgs[len(m.msgs)-1]
}

func (m *fakeMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.msgs)
}

var errFakeDB = errors.New("fake database failure")
