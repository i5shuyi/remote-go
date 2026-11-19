package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRejectSubstitutedPackage(t *testing.T) {
	dir := t.TempDir()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config
	cfg.Version = "1.2.3"
	cfg.Release.GitHub = "example/remote-go"
	name := "remote-go-1.2.3-android-arm64.apk"
	original := []byte("signed package")
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(private, original))
	write := func(bytes []byte) {
		t.Helper()
		entry := fmt.Sprintf(`<rss xmlns:s="http://www.andymatuschak.org/xml-namespaces/sparkle"><channel><item><enclosure url="https://github.com/example/remote-go/releases/download/v1.2.3/%s" length="%d" s:shortVersionString="1.2.3" s:sha256="%x" s:edSignature="%s"/></item></channel></rss>`, name, len(bytes), sha256.Sum256(bytes), sig)
		if err := os.WriteFile(filepath.Join(dir, name), bytes, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "appcast-android_arm64.xml"), []byte(entry), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(original)
	if err = verify(dir, "android_arm64", cfg, public); err != nil {
		t.Fatal(err)
	}
	write([]byte("replaced package"))
	if err = verify(dir, "android_arm64", cfg, public); err == nil {
		t.Fatal("accepted an unsigned replacement with an updated checksum")
	}
}
