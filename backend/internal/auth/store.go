package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/highlvmami/lumoraboard/backend/internal/proto"
)

// User is an account. One user may sign in through several providers.
type User struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Avatar string `json:"avatar,omitempty"`
}

// Profile is what a provider tells us about the person signing in.
type Profile struct {
	Subject string // stable id at the provider
	Name    string
	Avatar  string
}

// ErrNotFound is returned for a missing or expired session or invite.
var ErrNotFound = errors.New("auth: not found")

// Store keeps users, sessions and board access. Tokens are never stored,
// only their SHA-256 hashes, so a leaked table does not leak sessions.
type Store interface {
	// UpsertUser finds the user linked to provider+subject, creating one
	// on first sign-in, and refreshes the name and avatar.
	UpsertUser(ctx context.Context, provider string, p Profile) (User, error)
	CreateSession(ctx context.Context, tokenHash []byte, userID string, expires time.Time) error
	SessionUser(ctx context.Context, tokenHash []byte, now time.Time) (User, error)
	DeleteSession(ctx context.Context, tokenHash []byte) error

	// ClaimBoard returns the user's role on a board. The first user to
	// open a board nobody owns becomes its owner; that claim is atomic.
	ClaimBoard(ctx context.Context, board, userID string) (proto.Role, error)
	CreateInvite(ctx context.Context, tokenHash []byte, board string, role proto.Role, by string, expires time.Time) error
	// AcceptInvite grants the invite's role on its board, never lowering
	// a role the user already has, and returns the board and final role.
	AcceptInvite(ctx context.Context, tokenHash []byte, userID string, now time.Time) (string, proto.Role, error)
}

// newToken returns a random URL-safe token and its hash.
func newToken() (token string, hash []byte) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
	token = hex.EncodeToString(b[:])
	return token, hashToken(token)
}

// inviteAlphabet leaves out look-alikes (0/O, 1/I/L) so a code read
// aloud or typed from a screenshot comes out right.
const inviteAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// newInviteCode returns a short code people can type, such as
// "K7QM-2XRA", and its hash. Eight symbols from 31 give about 40 bits;
// with invites expiring after days, guessing one is not practical.
func newInviteCode() (code string, hash []byte) {
	// Bytes past the last whole multiple of the alphabet are redrawn so
	// every symbol is equally likely.
	limit := byte(256 / len(inviteAlphabet) * len(inviteAlphabet))
	out := make([]byte, 0, 9)
	var buf [16]byte
	for len(out) < 9 {
		if _, err := rand.Read(buf[:]); err != nil {
			panic("auth: crypto/rand failed: " + err.Error())
		}
		for _, v := range buf {
			if len(out) == 9 {
				break
			}
			if v >= limit {
				continue
			}
			if len(out) == 4 {
				out = append(out, '-')
			}
			out = append(out, inviteAlphabet[int(v)%len(inviteAlphabet)])
		}
	}
	code = string(out)
	return code, hashToken(normalizeInviteCode(code))
}

// normalizeInviteCode makes a typed code match the one issued: case,
// dashes and spaces do not matter.
func normalizeInviteCode(code string) string {
	code = strings.ToUpper(code)
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, code)
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// Memory is an in-process Store for tests and database-less runs.
type Memory struct {
	mu       sync.Mutex
	users    map[string]User
	accounts map[string]string // provider+"\x00"+subject -> user id
	sessions map[string]memSession
	owners   map[string]string                // board -> owner user id
	members  map[string]map[string]proto.Role // board -> user -> role
	invites  map[string]memInvite
}

type memSession struct {
	user    string
	expires time.Time
}

type memInvite struct {
	board   string
	role    proto.Role
	expires time.Time
}

// NewMemory creates an empty store.
func NewMemory() *Memory {
	return &Memory{
		users:    make(map[string]User),
		accounts: make(map[string]string),
		sessions: make(map[string]memSession),
		owners:   make(map[string]string),
		members:  make(map[string]map[string]proto.Role),
		invites:  make(map[string]memInvite),
	}
}

func (m *Memory) UpsertUser(_ context.Context, provider string, p Profile) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := provider + "\x00" + p.Subject
	id, ok := m.accounts[key]
	if !ok {
		id = newID()
		m.accounts[key] = id
	}
	u := User{ID: id, Name: p.Name, Avatar: p.Avatar}
	m.users[id] = u
	return u, nil
}

func (m *Memory) CreateSession(_ context.Context, hash []byte, userID string, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[string(hash)] = memSession{user: userID, expires: expires}
	return nil
}

func (m *Memory) SessionUser(_ context.Context, hash []byte, now time.Time) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[string(hash)]
	if !ok || !now.Before(s.expires) {
		return User{}, ErrNotFound
	}
	return m.users[s.user], nil
}

func (m *Memory) DeleteSession(_ context.Context, hash []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, string(hash))
	return nil
}

func (m *Memory) ClaimBoard(_ context.Context, board, userID string) (proto.Role, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	owner, ok := m.owners[board]
	if !ok {
		m.owners[board] = userID
		return proto.RoleOwner, nil
	}
	if owner == userID {
		return proto.RoleOwner, nil
	}
	return m.members[board][userID], nil
}

func (m *Memory) CreateInvite(_ context.Context, hash []byte, board string, role proto.Role, _ string, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invites[string(hash)] = memInvite{board: board, role: role, expires: expires}
	return nil
}

func (m *Memory) AcceptInvite(_ context.Context, hash []byte, userID string, now time.Time) (string, proto.Role, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invites[string(hash)]
	if !ok || !now.Before(inv.expires) {
		return "", "", ErrNotFound
	}
	if m.owners[inv.board] == userID {
		return inv.board, proto.RoleOwner, nil
	}
	if m.members[inv.board] == nil {
		m.members[inv.board] = make(map[string]proto.Role)
	}
	role := inv.role
	if cur := m.members[inv.board][userID]; cur.Rank() > role.Rank() {
		role = cur
	}
	m.members[inv.board][userID] = role
	return inv.board, role, nil
}
