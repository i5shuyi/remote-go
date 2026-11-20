package members

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"testing"

	"github.com/i5shuyi/remote-go/internal/tunnel"
)

var update = flag.Bool("update", false, "rewrite testdata/vectors.json")

// Vectors pin the byte formats other implementations (Android) must match.
type vectors struct {
	CreatorSeed  string `json:"creator_seed"`
	JoinerSeed   string `json:"joiner_seed"`
	GroupKey     string `json:"group_key"`
	GenesisBytes string `json:"genesis_signing_bytes"`
	GenesisSig   string `json:"genesis_sig"`
	GroupID      string `json:"group_id"`
	AddBytes     string `json:"add_signing_bytes"`
	AddID        string `json:"add_id"`
	MemberID     string `json:"joiner_member_id"`
	AddrPublic   string `json:"addr_region_304"`
	AddrCustom   string `json:"addr_custom"`
}

func TestVectors(t *testing.T) {
	creator := Identity{Priv: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))}
	joiner := Identity{Priv: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, 32))}
	k := bytes.Repeat([]byte{3}, 32)

	gen, err := sign(creator.Priv, TypeGenesis, Genesis{V: Version, Seed: bytes.Repeat([]byte{4}, 32), Creator: B64(creator.Public()), KeyFP: KeyFingerprint(k)})
	if err != nil {
		t.Fatal(err)
	}
	g, err := Load(gen, nil)
	if err != nil {
		t.Fatal(err)
	}
	in, _ := NewIntent(joiner, g.ID(), creator.ID(), ID{5})
	add, err := g.Add(creator, in)
	if err != nil {
		t.Fatal(err)
	}
	gid, ver, mid := g.ID(), g.State().KeyVersion, joiner.ID()
	addr := func(r Relay) string {
		a, err := tunnel.MemberEndpoint{GroupKey: k, GroupID: gid[:], KeyVersion: ver[:], MemberID: mid[:], Relay: r}.Addr()
		if err != nil {
			t.Fatal(err)
		}
		return string(a)
	}
	got := vectors{
		CreatorSeed:  hex.EncodeToString(bytes.Repeat([]byte{1}, 32)),
		JoinerSeed:   hex.EncodeToString(bytes.Repeat([]byte{2}, 32)),
		GroupKey:     hex.EncodeToString(k),
		GenesisBytes: string(SigningBytes(gen.Type, gen.Body)),
		GenesisSig:   hex.EncodeToString(gen.Sig),
		GroupID:      g.ID().String(),
		AddBytes:     string(SigningBytes(add.Type, add.Body)),
		AddID:        add.ID().String(),
		MemberID:     mid.String(),
		AddrPublic:   addr(Relay{Region: 304}),
		AddrCustom:   addr(Relay{Address: "derp.example.com:8443"}),
	}
	b, _ := json.MarshalIndent(got, "", "  ")
	if *update {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile("testdata/vectors.json", append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var w vectors
	json.Unmarshal(want, &w)
	if w != got {
		t.Fatalf("vectors changed:\n%s", b)
	}
}
