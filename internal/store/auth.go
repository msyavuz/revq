package store

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
)

const (
	DefaultUsername = "admin"
	DefaultPassword = "admin"
	MinPasswordLen  = 8

	pbkdf2Iterations = 600_000
	sessionTTL       = 30 * 24 * 3600
)

// User is the single account that can sign in.
type User struct {
	Username string `json:"username"`
	Salt     []byte `json:"salt"`
	Hash     []byte `json:"hash"`
	// MustChange is set while the account still has the default password.
	MustChange bool `json:"must_change"`
}

func hashPassword(password string, salt []byte) []byte {
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, 32)
	if err != nil {
		panic(err) // only fails on invalid parameters
	}
	return key
}

func (u *User) SetPassword(password string) {
	u.Salt = make([]byte, 16)
	_, _ = rand.Read(u.Salt)
	u.Hash = hashPassword(password, u.Salt)
}

func (u User) CheckPassword(password string) bool {
	return subtle.ConstantTimeCompare(hashPassword(password, u.Salt), u.Hash) == 1
}

// User returns the account, creating admin/admin on first use.
func (s *Store) User() (User, error) {
	var raw string
	var u User
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key='user'`).Scan(&raw); err == nil {
		if err := json.Unmarshal([]byte(raw), &u); err == nil && len(u.Hash) > 0 {
			return u, nil
		}
	}
	u = User{Username: DefaultUsername, MustChange: true}
	u.SetPassword(DefaultPassword)
	return u, s.SaveUser(u)
}

// ResetUser puts the account back to the default and signs everyone out.
func (s *Store) ResetUser() error {
	if _, err := s.db.Exec(`DELETE FROM settings WHERE key='user'`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM sessions`); err != nil {
		return err
	}
	_, err := s.User()
	return err
}

func (s *Store) SaveUser(u User) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES('user',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, toJSON(u))
	return err
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateSession returns a new session token. Only its hash is stored.
func (s *Store) CreateSession() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE expires < ?`, now())
	_, err := s.db.Exec(`INSERT INTO sessions(token_hash, expires) VALUES(?,?)`, tokenHash(token), now()+sessionTTL)
	return token, err
}

func (s *Store) SessionValid(token string) bool {
	if token == "" {
		return false
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash=? AND expires >= ?`, tokenHash(token), now()).Scan(&n)
	return n > 0
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash=?`, tokenHash(token))
	return err
}

// DeleteOtherSessions signs out everywhere except the given session.
func (s *Store) DeleteOtherSessions(keep string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash != ?`, tokenHash(keep))
	return err
}
