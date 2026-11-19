// Package liveupdate keeps the desktop host and session windows outside the
// installation directory and independent of the process the updater replaces.
package liveupdate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func runtimeDir() (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "remote-go", "session-runtime"), nil
}

// Resolve only a staged session executable. The local protocol never takes
// an arbitrary command or arguments from a frontend.
func windowHelper(name string) (string, error) {
	valid := name == "Remote Go Sessions"
	if runtime.GOOS == "windows" {
		hash := strings.TrimSuffix(strings.TrimPrefix(name, "sessions-"), ".exe")
		_, err := hex.DecodeString(hash)
		valid = len(hash) == sha256.Size*2 && err == nil && name == "sessions-"+hash+".exe"
	}
	if !valid {
		return "", fmt.Errorf("invalid session executable name")
	}
	dir, err := runtimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// Stage materializes a copy of the installed, already verified executable.
// On Windows an image mapped by a running process cannot be overwritten.
// On macOS a stable path preserves the helper's identity across replacements.
func Stage() (string, error) {
	src, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir, err := runtimeDir()
	if err != nil {
		return "", err
	}

	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(dir, ".stage-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	digest := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, digest), in)
	if err == nil {
		err = tmp.Chmod(0700)
	}
	closeErr := tmp.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	name := "Remote Go Sessions"
	if runtime.GOOS == "windows" {
		name = fmt.Sprintf("sessions-%x.exe", digest.Sum(nil))
	}
	dest := filepath.Join(dir, name)
	if existing, err := os.Open(dest); err == nil {
		hash := sha256.New()
		_, err = io.Copy(hash, existing)
		existing.Close()
		if err == nil && bytes.Equal(hash.Sum(nil), digest.Sum(nil)) {
			return dest, nil
		}
	}
	if err = os.Rename(tmp.Name(), dest); err != nil {
		return "", err
	}
	return dest, nil
}

// Windows keeps mapped images locked. Remove superseded, unused copies,
// leaving a newly staged image alone if another launch is just starting it.
func cleanOldHelpers(current string) {
	if runtime.GOOS != "windows" {
		return
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(current), "sessions-*.exe"))
	for _, path := range files {
		if path == current {
			continue
		}
		if _, err := windowHelper(filepath.Base(path)); err != nil {
			continue
		}
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && time.Since(info.ModTime()) > 24*time.Hour {
			_ = os.Remove(path)
		}
	}
}
