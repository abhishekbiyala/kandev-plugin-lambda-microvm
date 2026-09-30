// Package state persists the environments the provider owns. A MicroVM cannot be
// tagged, so this record is the provider's only way to find an environment again.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ErrInputMismatch reports an operation replayed with different inputs.
var ErrInputMismatch = errors.New("operation inputs changed")

// Environment is one owned environment, keyed by the host operation that allocated it.
// An empty Handle means the launch was reserved but its outcome is not yet recorded.
type Environment struct {
	OperationID string    `json:"operation_id"`
	InputDigest string    `json:"input_digest"`
	Handle      string    `json:"handle,omitempty"`
	ImageARN    string    `json:"image_arn"`
	Region      string    `json:"region"`
	ExpiresAt   time.Time `json:"expires_at"`
	// BootstrapJSON is the host's bootstrap descriptor, kept for the run hook.
	BootstrapJSON    string `json:"bootstrap_json,omitempty"`
	RunHookDelivered bool   `json:"run_hook_delivered,omitempty"`
}

// Store is a file-backed set of owned environments.
type Store struct {
	mu           sync.Mutex
	path         string
	environments map[string]Environment
}

// Open loads a store, or starts an empty one if the file does not exist. A corrupt
// file is an error, because overwriting it would lose running environments.
func Open(path string) (*Store, error) {
	s := &Store{path: path, environments: map[string]Environment{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(raw) == 0) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read environment state: %w", err)
	}
	var environments []Environment
	if err := json.Unmarshal(raw, &environments); err != nil {
		return nil, fmt.Errorf("environment state is corrupt: %w", err)
	}
	for _, environment := range environments {
		s.environments[environment.OperationID] = environment
	}
	return s, nil
}

// Reserve records an operation before its launch. It returns the existing record
// when the operation was already reserved, and ErrInputMismatch when it was reserved
// with a different input digest.
func (s *Store) Reserve(reservation Environment) (Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.environments[reservation.OperationID]; ok {
		if existing.InputDigest != reservation.InputDigest {
			return Environment{}, ErrInputMismatch
		}
		return existing, nil
	}
	s.environments[reservation.OperationID] = reservation
	return reservation, s.persistLocked()
}

// Complete records the launched environment's handle for a reserved operation.
func (s *Store) Complete(operationID, handle string) (Environment, error) {
	return s.update(func(e Environment) bool { return e.OperationID == operationID }, func(e *Environment) { e.Handle = handle })
}

// MarkRunHookDelivered records that the environment's run hook was posted.
func (s *Store) MarkRunHookDelivered(handle string) error {
	_, err := s.update(func(e Environment) bool { return e.Handle == handle }, func(e *Environment) { e.RunHookDelivered = true })
	return err
}

// Forget removes an operation's record.
func (s *Store) Forget(operationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.environments, operationID)
	return s.persistLocked()
}

// ByOperation looks up a record by operation identity.
func (s *Store) ByOperation(operationID string) (Environment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	environment, ok := s.environments[operationID]
	return environment, ok
}

// ByHandle looks up a launched environment by its MicroVM id.
func (s *Store) ByHandle(handle string) (Environment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, environment := range s.environments {
		if handle != "" && environment.Handle == handle {
			return environment, true
		}
	}
	return Environment{}, false
}

func (s *Store) update(match func(Environment) bool, change func(*Environment)) (Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, environment := range s.environments {
		if !match(environment) {
			continue
		}
		previous := environment
		change(&environment)
		s.environments[id] = environment
		if err := s.persistLocked(); err != nil {
			s.environments[id] = previous
			return Environment{}, err
		}
		return environment, nil
	}
	return Environment{}, errors.New("environment record not found")
}

// persistLocked writes the file atomically. Callers hold s.mu.
func (s *Store) persistLocked() error {
	environments := make([]Environment, 0, len(s.environments))
	for _, environment := range s.environments {
		environments = append(environments, environment)
	}
	sort.Slice(environments, func(i, j int) bool { return environments[i].OperationID < environments[j].OperationID })
	encoded, err := json.MarshalIndent(environments, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	temp := s.path + ".tmp"
	if err := os.WriteFile(temp, encoded, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, s.path)
}
