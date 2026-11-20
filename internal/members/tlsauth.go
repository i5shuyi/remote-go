package members

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// Certificate is a self-signed certificate for an Ed25519 identity. The
// certificate's names carry no authority: peers authorize the key alone.
// Renewal reuses the same key.
func Certificate(id Identity) (tls.Certificate, error) {
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "remote-go member " + id.ID().Short()},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, id.Public(), id.Priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: id.Priv}, nil
}

// PeerKey returns the Ed25519 key the peer proved possession of in the
// TLS 1.3 handshake.
func PeerKey(cs tls.ConnectionState) (ed25519.PublicKey, error) {
	if cs.Version != tls.VersionTLS13 {
		return nil, errors.New("需要 TLS 1.3")
	}
	if len(cs.PeerCertificates) == 0 {
		return nil, errors.New("对方未提供证书")
	}
	pub, ok := cs.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("对方证书不是 Ed25519")
	}
	return pub, nil
}

// baseTLS requires TLS 1.3 with a peer certificate on both sides and no
// session resumption, so every connection re-checks membership.
func baseTLS(id Identity, verify func(ed25519.PublicKey) error) (*tls.Config, error) {
	cert, err := Certificate(id)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		Certificates:           []tls.Certificate{cert},
		ClientAuth:             tls.RequireAnyClientCert,
		InsecureSkipVerify:     true, // chains are meaningless here; VerifyConnection checks the key
		SessionTicketsDisabled: true,
		ServerName:             "remote-go-member",
		VerifyConnection: func(cs tls.ConnectionState) error {
			pub, err := PeerKey(cs)
			if err != nil {
				return err
			}
			return verify(pub)
		},
	}, nil
}

// ServerTLS accepts any active member (or conflict peer, for sync).
func (n *Node) ServerTLS() (*tls.Config, error) {
	self, ok := n.Self()
	if !ok {
		return nil, ErrNotMember
	}
	return baseTLS(self, func(pub ed25519.PublicKey) error {
		_, err := n.Authorize(pub, PurposeSync, true)
		return err
	})
}

// ClientTLS accepts only the target member's key.
func (n *Node) ClientTLS(target ID, purpose string) (*tls.Config, error) {
	self, ok := n.Self()
	if !ok {
		return nil, ErrNotMember
	}
	return baseTLS(self, func(pub ed25519.PublicKey) error {
		if MemberID(pub) != target {
			return errors.New("身份不匹配：对方不是要连接的设备")
		}
		_, err := n.authorizeOutbound(pub, purpose)
		return err
	})
}

// authorizeOutbound allows syncing out after this device left, so its own
// removal can still reach the others.
func (n *Node) authorizeOutbound(pub ed25519.PublicKey, purpose string) (ID, error) {
	id, err := n.Authorize(pub, purpose, false)
	if errors.Is(err, ErrNotMember) && purpose == PurposeSync {
		n.mu.Lock()
		defer n.mu.Unlock()
		if n.departing && n.g != nil && n.g.State().Removed[n.self.ID()] && n.g.State().Active(id) {
			return id, nil
		}
	}
	return id, err
}

// PairContext is what the confirmation code binds, besides the TLS keys.
type PairContext struct {
	Token     B64    `json:"token"`
	Group     ID     `json:"group"`
	Inviter   B64    `json:"inviter"`
	Joiner    B64    `json:"joiner"`
	Direction string `json:"direction"` // "inviter-dials" | "joiner-dials"
}

// PairCode derives the 16-hex-digit code both people compare.
func PairCode(cs tls.ConnectionState, pc PairContext) (string, error) {
	c, err := marshalCanon(pc)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(c)
	b, err := cs.ExportKeyingMaterial("EXPORTER-remote-go-pair-v1", h[:], 8)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%04x-%04x-%04x-%04x", uint16(b[0])<<8|uint16(b[1]), uint16(b[2])<<8|uint16(b[3]),
		uint16(b[4])<<8|uint16(b[5]), uint16(b[6])<<8|uint16(b[7])), nil
}
