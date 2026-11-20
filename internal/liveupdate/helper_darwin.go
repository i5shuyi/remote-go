package liveupdate

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func fileDigest(path string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return digest, err
	}
	defer f.Close()
	h := sha256.New()
	_, err = io.Copy(h, f)
	h.Sum(digest[:0])
	return digest, err
}

func verifyBundle(bundle string) error {
	output, err := exec.Command("/usr/bin/codesign", "--verify", "--strict", bundle).CombinedOutput()
	if err != nil {
		return fmt.Errorf("verify session bundle: %w: %s", err, output)
	}
	return nil
}

// The executable's signature seals its Info.plist and bundle resources.
// Keep them together at a stable location independent of Sparkle's installation.
func stageHelper(src, dir string) (string, error) {
	macOS := filepath.Dir(src)
	contents := filepath.Dir(macOS)
	bundle := filepath.Dir(contents)
	if filepath.Base(macOS) != "MacOS" || filepath.Base(contents) != "Contents" || filepath.Ext(bundle) != ".app" {
		return stageExecutable(src, dir)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	cached := filepath.Join(dir, "Remote Go.app")
	entry := filepath.Join("Contents", "MacOS", filepath.Base(src))
	want, err := fileDigest(src)
	if err != nil {
		return "", err
	}
	// Older workers still launch new windows by this fixed protocol name.
	alias := filepath.Join(dir, "Remote Go Sessions")
	target := filepath.Join(filepath.Base(cached), entry)
	got, err := fileDigest(filepath.Join(cached, entry))
	reuse := err == nil && got == want && verifyBundle(cached) == nil
	if link, err := os.Readlink(alias); reuse && err == nil && link == target {
		return alias, nil
	}
	tmp, err := os.MkdirTemp(dir, ".stage-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if !reuse {
		stage := filepath.Join(tmp, "Remote Go.app")
		if output, err := exec.Command("/usr/bin/ditto", bundle, stage).CombinedOutput(); err != nil {
			return "", fmt.Errorf("stage session bundle: %w: %s", err, output)
		}
		if err := verifyBundle(stage); err != nil {
			return "", err
		}
		// Never expose a partial copy or temporarily remove the live path.
		if err := unix.RenamexNp(stage, cached, unix.RENAME_EXCL); errors.Is(err, unix.EEXIST) {
			if err := unix.RenamexNp(stage, cached, unix.RENAME_SWAP); err != nil {
				return "", err
			}
		} else if err != nil {
			return "", err
		}
	}
	link := filepath.Join(tmp, "helper")
	if err := os.Symlink(target, link); err != nil {
		return "", err
	}
	return alias, os.Rename(link, alias)
}
