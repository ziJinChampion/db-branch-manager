package lifecycle

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

type Scope struct {
	RepoID int64  `json:"repo_id"`
	Env    string `json:"environment"`
	PR     int64  `json:"pr"`
}

var (
	token      = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	resourceID = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

func (s Scope) Validate() error {
	if s.RepoID <= 0 || s.PR <= 0 || !ValidToken(s.Env) {
		return errors.New("positive repository and PR IDs and a valid environment are required")
	}
	return nil
}

func (s Scope) LockKey() int64 {
	hash := sha256.Sum256([]byte(fmt.Sprintf("dbbranch:v1:%d:%s:%d", s.RepoID, s.Env, s.PR)))
	return int64(binary.BigEndian.Uint64(hash[:8]))
}

func ValidToken(s string) bool      { return token.MatchString(s) }
func ValidID(s string) bool         { return resourceID.MatchString(s) }
func DatabaseName(id string) string { return "dbb_db_" + id }
func RoleName(id string) string     { return "dbb_role_" + id }

func NewID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}

type State string

const (
	Provisioning State = "provisioning"
	Ready        State = "ready"
	InUse        State = "in_use"
	Deleting     State = "deleting"
)

type Generation struct {
	Scope
	ID        string    `json:"id"`
	Revision  string    `json:"revision"`
	Baseline  string    `json:"baseline"`
	Attempt   string    `json:"attempt"`
	Database  string    `json:"database"`
	State     State     `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (g Generation) Collectible(cutoff time.Time) bool {
	return g.State != InUse && g.UpdatedAt.Before(cutoff)
}
