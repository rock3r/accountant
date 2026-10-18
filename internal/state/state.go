// Package state manages accountant's mutable state file (quota snapshots,
// session pins, cooldowns) with locked, atomic read-modify-write cycles.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"accountant/internal/lockfile"
)

// Window is one quota window sample (5-hour or weekly).
type Window struct {
	Utilization float64   `json:"utilization"`          // raw value as received
	ResetAt     time.Time `json:"reset_at,omitempty"`   // parsed window reset time
	Status      string    `json:"status,omitempty"`     // per-window status header, display only
	SampledAt   time.Time `json:"sampled_at,omitempty"` // local receipt time of the sample
	HasSample   bool      `json:"has_sample,omitempty"` // whether a sample was ever recorded
	// Normalized marks Utilization as already a fraction (the provider
	// converted its native scale); readers must not apply the configured
	// utilization_scale to it again.
	Normalized bool `json:"normalized,omitempty"`
}

// AccountState is the mutable per-account quota and health state.
type AccountState struct {
	FiveHour     Window    `json:"five_hour"`
	SevenDay     Window    `json:"seven_day"`
	Status       string    `json:"status,omitempty"`        // unified status header
	CoolingUntil time.Time `json:"cooling_until,omitempty"` // zero: not cooling
	AuthBroken   bool      `json:"auth_broken,omitempty"`
	UpdatedAt    time.Time `json:"updated_at,omitempty"`
}

// Pin binds a session to an account.
type Pin struct {
	Provider   string    `json:"provider"`
	SessionKey string    `json:"session_key"`
	AccountID  string    `json:"account_id"`
	LastSeen   time.Time `json:"last_seen"`
}

// State is the root of state.json.
type State struct {
	Accounts map[string]*AccountState `json:"accounts"`
	Pins     []Pin                    `json:"pins"`
}

// New returns an empty state.
func New() *State {
	return &State{Accounts: map[string]*AccountState{}}
}

// Account returns the state for id, creating it if needed.
func (s *State) Account(id string) *AccountState {
	if s.Accounts == nil {
		s.Accounts = map[string]*AccountState{}
	}
	a, ok := s.Accounts[id]
	if !ok {
		a = &AccountState{}
		s.Accounts[id] = a
	}
	return a
}

// FindPin returns the pin index for (provider, sessionKey), or -1.
func (s *State) FindPin(providerID, sessionKey string) int {
	for i := range s.Pins {
		if s.Pins[i].Provider == providerID && s.Pins[i].SessionKey == sessionKey {
			return i
		}
	}
	return -1
}

// SetPin creates or updates the pin for (provider, sessionKey).
func (s *State) SetPin(providerID, sessionKey, accountID string, now time.Time) {
	if i := s.FindPin(providerID, sessionKey); i >= 0 {
		s.Pins[i].AccountID = accountID
		s.Pins[i].LastSeen = now
		return
	}
	s.Pins = append(s.Pins, Pin{Provider: providerID, SessionKey: sessionKey, AccountID: accountID, LastSeen: now})
}

// DropPinsFor removes every pin pointing at accountID.
func (s *State) DropPinsFor(accountID string) {
	kept := s.Pins[:0]
	for _, p := range s.Pins {
		if p.AccountID != accountID {
			kept = append(kept, p)
		}
	}
	s.Pins = kept
}

// Store owns the state file and its lock file.
type Store struct {
	stateFile string
	lockFile  string
}

// NewStore creates a store rooted at the given state file path.
func NewStore(stateFile string) *Store {
	return &Store{
		stateFile: stateFile,
		lockFile:  filepath.Join(filepath.Dir(stateFile), "state.lock"),
	}
}

// File returns the state file path.
func (s *Store) File() string { return s.stateFile }

// Load reads the state file. A missing file yields an empty state.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return New(), nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return New(), nil
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if st.Accounts == nil {
		st.Accounts = map[string]*AccountState{}
	}
	return &st, nil
}

// Save writes the state atomically (temp file, fsync, rename, 0600).
func Save(path string, st *State) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Locked runs fn with the shared lock file held: it loads fresh state,
// hands it to fn, and saves atomically when fn reports dirty and returns
// a nil error. All read-modify-write cycles of state go through Locked,
// by the daemon and by CLI commands alike.
func (s *Store) Locked(fn func(*State) (dirty bool, err error)) error {
	return lockfile.Locked(s.lockFile, func() error {
		st, err := Load(s.stateFile)
		if err != nil {
			return err
		}
		dirty, err := fn(st)
		if err != nil {
			return err
		}
		if !dirty {
			return nil
		}
		return Save(s.stateFile, st)
	})
}
