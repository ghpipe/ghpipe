package hostfs

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNormalizeTextCRLF(t *testing.T) {
	in := []byte("a\r\nb\r\nc\n")
	got := string(NormalizeText(in))
	if got != "a\nb\nc\n" {
		t.Fatalf("normalize = %q", got)
	}
	// CRLF and LF checkouts must hash the same.
	if string(NormalizeText([]byte("x\ny\n"))) != got[:0]+"x\ny\n" {
		t.Fatalf("LF input changed")
	}
}

func TestReplaceIsAtomicAndOverwrites(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")
	if err := Replace(target, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Replace(target, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "two" {
		t.Fatalf("content = %q", data)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestReplaceHonoursMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "secret")
	if err := Replace(target, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestCheckSecretFlagsLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode-bit check is POSIX only")
	}
	dir := t.TempDir()
	loose := filepath.Join(dir, "loose")
	if err := os.WriteFile(loose, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckSecret(loose); err == nil {
		t.Fatal("0644 secret must be reported as not protected")
	}
	tight := filepath.Join(dir, "tight")
	if err := os.WriteFile(tight, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	protection, err := CheckSecret(tight)
	if err != nil {
		t.Fatalf("0600 secret rejected: %v", err)
	}
	if protection != ProtectionPOSIX0600 {
		t.Fatalf("protection = %q", protection)
	}
}

func TestLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.lock")
	first, err := Acquire(path, LockInfo{PID: os.Getpid(), Purpose: "test"})
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.Release()
	if _, err := Acquire(path, LockInfo{PID: os.Getpid()}); err == nil {
		t.Fatal("second acquire must fail while the lock is held")
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	second, err := Acquire(path, LockInfo{PID: os.Getpid()})
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	second.Release()
}
