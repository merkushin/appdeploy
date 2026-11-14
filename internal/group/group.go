// Package group is the membership and trust of a set of servers: a private CA
// created by the first server, one-time invite tokens, and mutual TLS between
// the agents. There is no consensus: the server list is versioned and the
// higher version wins.
package group

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Server is a member of the group.
type Server struct {
	Name   string   `json:"name"`
	Addr   string   `json:"addr"` // host:port of its peer API
	Labels []string `json:"labels"`
}

// HasLabel reports whether the server carries any of the labels.
func (s Server) HasLabel(labels ...string) bool {
	for _, l := range labels {
		for _, m := range s.Labels {
			if l == m {
				return true
			}
		}
	}
	return false
}

// Protocol is the version of what servers say to each other. Servers refuse to
// coordinate deploys with servers that speak another one, which only happens
// while a group is being updated; bump it when a change is not backward compatible.
const Protocol = 1

// State is the replicated membership.
type State struct {
	Version int64    `json:"version"`
	Name    string   `json:"name"`
	Servers []Server `json:"servers"`
	// DesiredVersion is the release every server should run (empty: no opinion).
	// Servers install it themselves, one at a time; see update.Decide.
	DesiredVersion string `json:"desired_version,omitempty"`
}

// Group is this server's view of its group.
type Group struct {
	Dir  string
	Self string

	mu      sync.Mutex
	state   State
	caCert  *x509.Certificate
	caKey   *ecdsa.PrivateKey // only on the founder, which alone can invite
	node    tls.Certificate
	pool    *x509.CertPool
	invites map[string]time.Time // sha256(secret) -> expiry
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode)
}

func newKey() (*ecdsa.PrivateKey, error) { return ecdsa.GenerateKey(elliptic.P256(), rand.Reader) }

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}

func sans(addr, name string) (ips []net.IP, dns []string) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if ip := net.ParseIP(host); ip != nil {
		ips = append(ips, ip)
	} else if host != "" {
		dns = append(dns, host)
	}
	return ips, append(dns, name)
}

func signNode(caCert *x509.Certificate, caKey *ecdsa.PrivateKey, pub any, name, addr string) ([]byte, error) {
	ips, dns := sans(addr, name)
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: name, Organization: []string{"appdeploy"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(5, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses: ips, DNSNames: dns,
	}
	return x509.CreateCertificate(rand.Reader, tmpl, caCert, pub, caKey)
}

// Init creates a new group with this server as its founder and CA.
func Init(dir, groupName, self, advertise string, labels []string) (*Group, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, "group.json")); err == nil {
		return nil, errors.New("this server already belongs to a group")
	}
	caKey, err := newKey()
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "appdeploy group " + groupName},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, _ := x509.ParseCertificate(caDER)
	nodeKey, err := newKey()
	if err != nil {
		return nil, err
	}
	nodeDER, err := signNode(caCert, caKey, &nodeKey.PublicKey, self, advertise)
	if err != nil {
		return nil, err
	}
	caKeyDER, _ := x509.MarshalECPrivateKey(caKey)
	nodeKeyDER, _ := x509.MarshalECPrivateKey(nodeKey)
	for _, f := range []struct {
		name, typ string
		der       []byte
		mode      os.FileMode
	}{
		{"ca.crt", "CERTIFICATE", caDER, 0o644}, {"ca.key", "EC PRIVATE KEY", caKeyDER, 0o600},
		{"node.crt", "CERTIFICATE", nodeDER, 0o644}, {"node.key", "EC PRIVATE KEY", nodeKeyDER, 0o600},
	} {
		if err := writePEM(filepath.Join(dir, f.name), f.typ, f.der, f.mode); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "self"), []byte(self), 0o600); err != nil {
		return nil, err
	}
	g := &Group{Dir: dir, Self: self}
	g.state = State{Version: 1, Name: groupName, Servers: []Server{{Name: self, Addr: advertise, Labels: labels}}}
	if err := g.save(); err != nil {
		return nil, err
	}
	return Load(dir, self)
}

// Load reads the group of this server; (nil, nil) if it has none.
func Load(dir, self string) (*Group, error) {
	b, err := os.ReadFile(filepath.Join(dir, "group.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if self == "" {
		sb, err := os.ReadFile(filepath.Join(dir, "self"))
		if err != nil {
			return nil, err
		}
		self = string(bytes.TrimSpace(sb))
	}
	g := &Group{Dir: dir, Self: self, invites: map[string]time.Time{}}
	if err := json.Unmarshal(b, &g.state); err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	g.pool = x509.NewCertPool()
	if !g.pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("bad ca.crt")
	}
	blk, _ := pem.Decode(caPEM)
	if g.caCert, err = x509.ParseCertificate(blk.Bytes); err != nil {
		return nil, err
	}
	if g.node, err = tls.LoadX509KeyPair(filepath.Join(dir, "node.crt"), filepath.Join(dir, "node.key")); err != nil {
		return nil, err
	}
	// Present the CA with the node certificate so that a joining server can pin it.
	g.node.Certificate = append(g.node.Certificate, g.caCert.Raw)
	if kb, err := os.ReadFile(filepath.Join(dir, "ca.key")); err == nil {
		blk, _ := pem.Decode(kb)
		if blk != nil {
			g.caKey, _ = x509.ParseECPrivateKey(blk.Bytes)
		}
	}
	return g, nil
}

func (g *Group) save() error {
	b, _ := json.MarshalIndent(g.state, "", "  ")
	tmp := filepath.Join(g.Dir, ".group.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(g.Dir, "group.json"))
}

// State returns a copy of the membership.
func (g *Group) State() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.state
	s.Servers = append([]Server(nil), g.state.Servers...)
	return s
}

// Me is this server's entry.
func (g *Group) Me() Server {
	for _, s := range g.State().Servers {
		if s.Name == g.Self {
			return s
		}
	}
	return Server{Name: g.Self}
}

// Peers lists the other servers, sorted by name.
func (g *Group) Peers() []Server {
	var out []Server
	for _, s := range g.State().Servers {
		if s.Name != g.Self {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// IsFounder reports whether this server holds the CA and can invite.
func (g *Group) IsFounder() bool { return g.caKey != nil }

// Apply adopts a membership state if its version is higher. It reports whether it changed.
func (g *Group) Apply(st State) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if st.Version <= g.state.Version {
		return false, nil
	}
	g.state = st
	return true, g.save()
}

// AddServer adds or replaces a server (founder side) and bumps the version.
func (g *Group) AddServer(s Server) (State, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var list []Server
	for _, x := range g.state.Servers {
		if x.Name != s.Name {
			list = append(list, x)
		}
	}
	g.state.Servers = append(list, s)
	g.state.Version++
	return g.state, g.save()
}

// SetDesiredVersion records the release the group should run and bumps the version.
func (g *Group) SetDesiredVersion(v string) (State, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state.DesiredVersion = v
	g.state.Version++
	return g.state, g.save()
}

// RemoveServer removes a server by name and bumps the version.
func (g *Group) RemoveServer(name string) (State, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var list []Server
	for _, x := range g.state.Servers {
		if x.Name != name {
			list = append(list, x)
		}
	}
	if len(list) == len(g.state.Servers) {
		return g.state, fmt.Errorf("no server named %q", name)
	}
	g.state.Servers = list
	g.state.Version++
	return g.state, g.save()
}

// ---- TLS ----

// ServerTLS is the config of the peer listener. Client certificates are
// verified when presented; handlers must require them (except for join).
func (g *Group) ServerTLS() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{g.node},
		ClientCAs: g.pool, ClientAuth: tls.VerifyClientCertIfGiven,
	}
}

// Client returns an HTTP client that presents this server's certificate and
// trusts only the group CA.
func (g *Group) Client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{g.node}, RootCAs: g.pool,
	}, MaxIdleConnsPerHost: 4}}
}

// VerifiedPeer reports whether the request came with a client certificate signed by the group CA.
func VerifiedPeer(r *http.Request) (string, bool) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return "", false
	}
	return r.TLS.PeerCertificates[0].Subject.CommonName, true
}

// ---- invites and joining ----

type token struct {
	Addr   string `json:"a"`
	FP     string `json:"f"` // sha256 of the CA certificate
	Secret string `json:"s"`
}

func fingerprint(c *x509.Certificate) string {
	h := sha256.Sum256(c.Raw)
	return hex.EncodeToString(h[:])
}

// Invite creates a one-time token a new server uses to join. Founder only.
func (g *Group) Invite(ttl time.Duration) (string, error) {
	if !g.IsFounder() {
		return "", errors.New("only the server that created the group can invite new servers")
	}
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	secret := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(secret))
	g.mu.Lock()
	g.invites[hex.EncodeToString(sum[:])] = time.Now().Add(ttl)
	g.mu.Unlock()
	b, _ := json.Marshal(token{Addr: g.Me().Addr, FP: fingerprint(g.caCert), Secret: secret})
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (g *Group) consumeInvite(secret string) bool {
	sum := sha256.Sum256([]byte(secret))
	key := hex.EncodeToString(sum[:])
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, exp := range g.invites {
		if time.Now().After(exp) {
			delete(g.invites, k)
		}
	}
	exp, ok := g.invites[key]
	if !ok || time.Now().After(exp) {
		return false
	}
	delete(g.invites, key)
	return true
}

type joinRequest struct {
	Secret string   `json:"secret"`
	Name   string   `json:"name"`
	Addr   string   `json:"addr"`
	Labels []string `json:"labels"`
	CSR    []byte   `json:"csr"`
}

type joinResponse struct {
	CA    []byte `json:"ca"`
	Cert  []byte `json:"cert"`
	State State  `json:"state"`
}

// HandleJoin serves POST /peer/v1/join on the founder. It returns the new
// membership so the caller can broadcast it.
func (g *Group) HandleJoin(w http.ResponseWriter, r *http.Request) (State, bool) {
	fail := func(code int, msg string) (State, bool) { http.Error(w, msg, code); return State{}, false }
	if !g.IsFounder() {
		return fail(http.StatusForbidden, "this server cannot sign certificates")
	}
	var req joinRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		return fail(400, "bad request")
	}
	if !g.consumeInvite(req.Secret) {
		return fail(http.StatusForbidden, "invalid or expired invite")
	}
	if req.Name == "" || req.Addr == "" {
		return fail(400, "name and addr are required")
	}
	csr, err := x509.ParseCertificateRequest(req.CSR)
	if err != nil || csr.CheckSignature() != nil {
		return fail(400, "bad certificate request")
	}
	der, err := signNode(g.caCert, g.caKey, csr.PublicKey, req.Name, req.Addr)
	if err != nil {
		return fail(500, err.Error())
	}
	st, err := g.AddServer(Server{Name: req.Name, Addr: req.Addr, Labels: req.Labels})
	if err != nil {
		return fail(500, err.Error())
	}
	resp := joinResponse{
		CA:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: g.caCert.Raw}),
		Cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), State: st,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
	return st, true
}

// Join enrolls this server into the group the token belongs to.
func Join(ctx context.Context, dir, tok, self, advertise string, labels []string) (*Group, error) {
	if _, err := os.Stat(filepath.Join(dir, "group.json")); err == nil {
		return nil, errors.New("this server already belongs to a group")
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return nil, errors.New("malformed invite token")
	}
	var t token
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, errors.New("malformed invite token")
	}
	key, err := newKey()
	if err != nil {
		return nil, err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: self}}, key)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(joinRequest{Secret: t.Secret, Name: self, Addr: advertise, Labels: labels, CSR: csrDER})
	// The CA is not known yet: trust exactly the CA whose fingerprint is in the token.
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) < 2 {
				return errors.New("the founder did not present its CA certificate")
			}
			ca, err := x509.ParseCertificate(raw[len(raw)-1])
			if err != nil {
				return err
			}
			if subtle.ConstantTimeCompare([]byte(fingerprint(ca)), []byte(t.FP)) != 1 {
				return errors.New("the server's CA does not match the invite (wrong server?)")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			pool := x509.NewCertPool()
			pool.AddCert(ca)
			_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
			return err
		},
	}}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+t.Addr+"/peer/v1/join", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contacting %s: %w", t.Addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("join refused: %s", bytes.TrimSpace(msg))
	}
	var jr joinResponse
	if err := json.NewDecoder(resp.Body).Decode(&jr); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	if err := writePEM(filepath.Join(dir, "node.key"), "EC PRIVATE KEY", keyDER, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), jr.CA, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "node.crt"), jr.Cert, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "self"), []byte(self), 0o600); err != nil {
		return nil, err
	}
	g := &Group{Dir: dir, Self: self, state: jr.State}
	if err := g.save(); err != nil {
		return nil, err
	}
	return Load(dir, self)
}
