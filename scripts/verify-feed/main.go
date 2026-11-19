// verify-feed checks the exact files a release publisher is about to upload.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

type config struct {
	Version string
	Release struct{ GitHub string }
	Updates struct{ Key string }
}

type enclosure struct {
	URL       string `xml:"url,attr"`
	Length    int    `xml:"length,attr"`
	Version   string `xml:"http://www.andymatuschak.org/xml-namespaces/sparkle shortVersionString,attr"`
	Signature string `xml:"http://www.andymatuschak.org/xml-namespaces/sparkle edSignature,attr"`
	Hash      string `xml:"http://www.andymatuschak.org/xml-namespaces/sparkle sha256,attr"`
}

func verify(dir, target string, cfg config, key ed25519.PublicKey) error {
	data, err := os.ReadFile(filepath.Join(dir, "appcast-"+target+".xml"))
	if err != nil {
		return err
	}
	var rss struct {
		Entries []enclosure `xml:"channel>item>enclosure"`
	}
	if err = xml.Unmarshal(data, &rss); err != nil {
		return err
	}
	if len(rss.Entries) != 1 {
		return fmt.Errorf("%s: expected one installer", target)
	}
	e := rss.Entries[0]
	osName, _, _ := strings.Cut(target, "_")
	suffix := map[string]string{"macos": ".dmg", "windows": "-setup.exe", "android": ".apk"}[osName]
	name := fmt.Sprintf("remote-go-%s-%s%s", cfg.Version, strings.ReplaceAll(target, "_", "-"), suffix)
	url := fmt.Sprintf("https://github.com/%s/releases/download/v%s/%s", cfg.Release.GitHub, cfg.Version, name)
	if suffix == "" || e.URL != url || e.Version != cfg.Version {
		return fmt.Errorf("%s: incorrect version or installer URL", target)
	}
	installer, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(installer)
	sig, err := base64.StdEncoding.DecodeString(e.Signature)
	if err != nil || e.Length != len(installer) || e.Hash != hex.EncodeToString(sum[:]) || !ed25519.Verify(key, installer, sig) {
		return fmt.Errorf("%s: installer integrity or signature check failed", target)
	}
	fmt.Printf("Verified %s\n", name)
	return nil
}

func run() error {
	if len(os.Args) < 3 {
		return fmt.Errorf("usage: verify-feed RELEASE_DIRECTORY TARGET...")
	}
	data, err := os.ReadFile("nsgo.yaml")
	if err != nil {
		return err
	}
	var cfg config
	if err = yaml.Unmarshal(data, &cfg); err != nil {
		return err
	}
	key, err := base64.StdEncoding.DecodeString(cfg.Updates.Key)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid updates.key")
	}
	for _, target := range os.Args[2:] {
		if err := verify(os.Args[1], target, cfg, key); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
