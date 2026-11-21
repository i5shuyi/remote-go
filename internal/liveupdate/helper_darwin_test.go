package liveupdate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageSignedSessionBundle(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "Source.app")
	contents := filepath.Join(bundle, "Contents")
	exe := filepath.Join(contents, "MacOS", "Remote Go")
	if err := os.MkdirAll(filepath.Dir(exe), 0700); err != nil {
		t.Fatal(err)
	}
	probe := exec.Command("/usr/bin/clang", "-framework", "Foundation", "-x", "objective-c", "-", "-o", exe)
	probe.Stdin = strings.NewReader(`#import <Foundation/Foundation.h>
int main(void) {
	@autoreleasepool { puts(NSBundle.mainBundle.bundleIdentifier.UTF8String ?: "(none)"); }
	return 0;
}`)
	if out, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("compile bundle identity probe: %v: %s", err, out)
	}
	t.Setenv("HOME", t.TempDir())
	cache, err := runtimeDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(cache, "Remote Go Sessions")
	if err := os.WriteFile(alias, []byte("old standalone helper"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"1", "2"} {
		plist := fmt.Sprintf("<plist version=\"1.0\"><dict><key>CFBundleIdentifier</key><string>dev.remote-go.test</string><key>CFBundleExecutable</key><string>Remote Go</string><key>CFBundleVersion</key><string>%s</string></dict></plist>", version)
		if err := os.WriteFile(filepath.Join(contents, "Info.plist"), []byte(plist), 0600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("/usr/bin/codesign", "--force", "--sign", "-", bundle).CombinedOutput(); err != nil {
			t.Fatalf("sign bundle: %v: %s", err, out)
		}
		path, err := stageHelper(exe, cache)
		if err != nil || path != alias {
			t.Fatalf("stage: %q, %v", path, err)
		}
		launch, err := windowHelper(filepath.Base(path))
		if err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(launch).CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "dev.remote-go.test" {
			t.Fatalf("running helper lost bundle identity: %v: %s", err, out)
		}
		if err := verifyBundle(path); err != nil {
			t.Fatal(err)
		}
		want, err := fileDigest(exe)
		got, cachedErr := fileDigest(path)
		if err != nil || cachedErr != nil || got != want {
			t.Fatal("staged helper does not match the signed release")
		}
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stageHelper(exe, cache); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(path)
		if err != nil || !os.SameFile(before, after) {
			t.Fatal("replaced an identical signed helper on the next launch")
		}
	}
}
