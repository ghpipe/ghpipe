// Package hostfs is the only place in the codebase that touches platform
// specifics: file locking, atomic replacement, secret-file protection checks,
// link/entry creation and text normalisation. Everything else stays portable.
package hostfs

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// LockInfo describes who holds a lock, so a conflict message can say whether
// the holder is a live process or a stale file.
type LockInfo struct {
	PID       int    `json:"pid"`
	SubjectID string `json:"subject_id,omitempty"`
	Purpose   string `json:"purpose,omitempty"`
	Acquired  string `json:"acquired,omitempty"`
}

// Lock is an exclusive advisory lock on a file.
type Lock struct {
	file *os.File
	path string
}

// Acquire takes an exclusive non-blocking lock, writing holder information into
// the lock file for diagnostics. Returns ErrLocked when another holder exists.
func Acquire(path string, info LockInfo) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := lockFile(f); err != nil {
		held := readHolder(path)
		f.Close()
		if held.PID != 0 {
			return nil, fmt.Errorf("locked by pid %d%s", held.PID, holderSuffix(held))
		}
		return nil, err
	}
	if info.Acquired == "" {
		info.Acquired = time.Now().UTC().Format(time.RFC3339)
	}
	// Best-effort diagnostics: truncate and rewrite the holder record.
	if payload, err := json.Marshal(info); err == nil {
		_ = f.Truncate(0)
		_, _ = f.Seek(0, 0)
		_, _ = f.Write(append(payload, '\n'))
	}
	return &Lock{file: f, path: path}, nil
}

func holderSuffix(info LockInfo) string {
	s := ""
	if info.Purpose != "" {
		s += " purpose=" + info.Purpose
	}
	if info.SubjectID != "" {
		s += " subject=" + info.SubjectID
	}
	return s
}

func readHolder(path string) LockInfo {
	var info LockInfo
	data, err := os.ReadFile(path)
	if err != nil {
		return info
	}
	_ = json.Unmarshal(data, &info)
	return info
}

// Release drops the lock. Safe to call more than once.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := unlockFile(l.file)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
}
