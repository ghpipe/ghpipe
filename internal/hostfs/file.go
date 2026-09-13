package hostfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Protection describes how well a secret file is protected on this platform.
type Protection string

const (
	ProtectionPOSIX0600 Protection = "posix_0600"
	ProtectionUserProfile Protection = "user_profile_acl"
	ProtectionUnknown   Protection = "unknown"
)

// CheckSecret reports whether a secret file is protected. POSIX requires mode
// 0600 (no group/other bits); Windows has no POSIX mode, so the expectation is
// that the file lives inside the user profile, whose ACL already restricts it.
// The result is reported, never assumed.
func CheckSecret(path string) (Protection, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ProtectionUnknown, err
	}
	if info.IsDir() {
		return ProtectionUnknown, errors.New("secret path is a directory")
	}
	if runtime.GOOS != "windows" {
		if info.Mode().Perm()&0o077 != 0 {
			return ProtectionPOSIX0600, errors.New("secret file is accessible to group or others (use 0600)")
		}
		return ProtectionPOSIX0600, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ProtectionUnknown, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ProtectionUnknown, err
	}
	if !strings.HasPrefix(strings.ToLower(abs), strings.ToLower(home)) {
		return ProtectionUnknown, errors.New("secret file is outside the user profile")
	}
	return ProtectionUserProfile, nil
}

// Replace writes data to path atomically: a temp file in the same directory,
// synced, then renamed over the target. On Windows the rename can fail while
// the target is briefly held open, so it is retried with backoff.
func Replace(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	var last error
	for attempt := 0; attempt < 5; attempt++ {
		if last = os.Rename(tmpName, path); last == nil {
			return nil
		}
		if runtime.GOOS != "windows" {
			break
		}
		time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
	}
	return last
}

// NormalizeText rewrites CRLF to LF so the same resource hashes identically on
// a checkout that converted line endings.
func NormalizeText(data []byte) []byte {
	if !strings.Contains(string(data), "\r\n") {
		return data
	}
	return []byte(strings.ReplaceAll(string(data), "\r\n", "\n"))
}

// Exists reports whether a path exists (following links).
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// IsNotExist reports fs.ErrNotExist wrapped errors.
func IsNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
