package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

// loadKey reads ssh_key.pem next to the exe (ECDSA P-256, created on first start) and returns the signer and the
// public key line that travels to the container in an env variable.
func loadKey(path string) (ssh.Signer, string, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		raw, err = newKeyPEM()
		if err == nil {
			err = os.WriteFile(path, raw, 0o600)
		}
	}
	if err != nil {
		return nil, "", err
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return nil, "", fmt.Errorf("ssh_key.pem: %w", err)
	}
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " " + Label
	return signer, pub, nil
}

func newKeyPEM() ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), err
}

// tunnel is one SSH session to the machine with the local ports forwarded through it.
type tunnel struct {
	client    *ssh.Client
	listeners []net.Listener
	closed    atomic.Bool
	requests  atomic.Int64 // connections accepted on the API port since the last statistics line
	lastUse   atomic.Int64 // unix seconds of the last one
}

// dialSSH connects as root with the program's key. An empty pinned fingerprint trusts the first key seen
// (a machine rented a minute ago); the fingerprint is returned to be pinned for reconnects.
func dialSSH(ctx context.Context, addr string, signer ssh.Signer, pinned string) (*ssh.Client, string, error) {
	seen := ""
	cfg := &ssh.ClientConfig{
		User: "root", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, Timeout: 15 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			seen = strings.TrimPrefix(ssh.FingerprintSHA256(key), "SHA256:")
			if pinned != "" && pinned != seen {
				return fmt.Errorf("ключ SSH машины изменился")
			}
			return nil
		},
	}
	conn, err := (&net.Dialer{Timeout: cfg.Timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, "", err
	}
	conn.SetDeadline(time.Now().Add(cfg.Timeout))
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, "", err
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(sc, chans, reqs), seen, nil
}

func newTunnel(client *ssh.Client, onDrop func(error)) *tunnel {
	t := &tunnel{client: client}
	t.lastUse.Store(time.Now().Unix())
	go func() { // a dead session is the usual reason a client sees ECONNREFUSED: report it at once
		err := client.Wait()
		if !t.closed.Swap(true) {
			onDrop(err)
		}
	}()
	go t.keepAlive()
	return t
}

// keepAlive closes the session when the machine stops answering, so Wait above fires and the tick reconnects.
func (t *tunnel) keepAlive() {
	for !t.closed.Load() {
		time.Sleep(20 * time.Second)
		answered := make(chan error, 1)
		go func() {
			_, _, err := t.client.SendRequest("keepalive@openssh.com", true, nil)
			answered <- err
		}()
		select {
		case err := <-answered:
			if err != nil {
				t.client.Close()
				return
			}
		case <-time.After(15 * time.Second):
			t.client.Close()
			return
		}
	}
}

// forward listens on 127.0.0.1:localPort and pipes every connection to 127.0.0.1:remotePort on the machine.
func (t *tunnel) forward(localPort, remotePort int, counted bool) error {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		return err
	}
	t.listeners = append(t.listeners, l)
	go func() {
		for {
			local, err := l.Accept()
			if err != nil {
				return // listener closed
			}
			if counted {
				t.requests.Add(1)
				t.lastUse.Store(time.Now().Unix())
			}
			go func() {
				defer local.Close()
				remote, err := t.client.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", remotePort))
				if err != nil {
					return // the client gets a closed connection; a dead session is reported by Wait
				}
				defer remote.Close()
				go io.Copy(remote, local)
				io.Copy(local, remote)
			}()
		}
	}()
	return nil
}

func (t *tunnel) alive() bool { return t != nil && !t.closed.Load() }

func (t *tunnel) close() {
	if t == nil {
		return
	}
	t.closed.Store(true)
	for _, l := range t.listeners {
		l.Close()
	}
	t.client.Close()
}

// exec runs a short command on the machine.
func (t *tunnel) exec(cmd string) (string, error) {
	if !t.alive() {
		return "", fmt.Errorf("нет связи с машиной")
	}
	s, err := t.client.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	done := make(chan struct{})
	var out []byte
	go func() { out, err = s.Output(cmd); close(done) }()
	select {
	case <-done:
		return strings.TrimSpace(string(out)), err
	case <-time.After(20 * time.Second):
		return "", fmt.Errorf("команда не ответила за 20 с")
	}
}
