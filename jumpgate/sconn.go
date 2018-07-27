package main

import (
	"context"
	"fmt"
	"net"
	"strings"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

type sconn struct {
	conn net.Conn
	cfg  ssh.ServerConfig

	// Filled in after handshake.
	password string
	target   string
	meta     ssh.ConnMetadata
	key      ssh.PublicKey
	user     string
	client   ssh.Conn
}

func (sc *sconn) pubkeyCallback(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	log.Infof("Pubkey attempt by %s from %s: %v", meta.User(), meta.RemoteAddr().String(), ssh.FingerprintSHA256(key))
	t := strings.SplitN(meta.User(), "%", 2)
	if len(t) < 2 {
		// TODO: show error message to user
		return nil, fmt.Errorf("username has wrong format: %q", meta.User())
	}
	user, target := t[0], t[1]
	log.Infof("... %q connecting to %q", user, target)
	sc.meta = meta
	sc.key = key
	sc.user = user
	sc.target = target
	if err := db.QueryRowContext(
		context.TODO(),
		`SELECT password FROM passwords WHERE target=$1`,
		target).Scan(&sc.password); err != nil {
		return nil, fmt.Errorf("getting password for %q: %v", target, err)
	}
	return nil, nil
}

func (sc *sconn) keyboardInteractive(user, instruction string, questions []string, echos []bool) (answers []string, err error) {
	log.Printf("... keyboardinteractive: %q %q %q %v", user, instruction, questions, echos)
	var ans []string
	for _ = range questions {
		ans = append(ans, sc.password)
	}
	return ans, nil
}

func (sc *sconn) hostKeyCallback(hostname string, remote net.Addr, key ssh.PublicKey) error {
	// TODO: check host cert, if present.
	log.Printf("... Host key %q: %v", hostname, ssh.FingerprintSHA256(key))
	var k string
	if err := db.QueryRowContext(
		context.TODO(),
		`SELECT pubkey FROM host_keys WHERE target=$1`,
		sc.target).Scan(&k); err != nil {
		return fmt.Errorf("getting host key %q: %v", sc.target, err)
	}
	if got, want := ssh.FingerprintSHA256(key), k; got != want {
		return fmt.Errorf("wrong host key. got %q, want %q", got, want)
	}
	return nil
}

// TODO: time out with ctx.
func (sc *sconn) handleConnection(ctx context.Context) error {
	sc.cfg.PublicKeyCallback = sc.pubkeyCallback
	conn, chch, requestch, err := ssh.NewServerConn(sc.conn, &sc.cfg)
	if err != nil {
		return err
	}
	defer conn.Close()
	log.Infof("... Server connected!")

	var n int
	if err := db.QueryRowContext(
		ctx,
		`SELECT 1 FROM acl WHERE pubkey=$1 AND target=$2`,
		ssh.FingerprintSHA256(sc.key),
		sc.target).Scan(&n); err != nil {
		return fmt.Errorf("acl rejects key %q from connecting to %q", ssh.FingerprintSHA256(sc.key), sc.target)
	}

	log.Infof("... dialing %q", sc.target)
	sc.client, err = ssh.Dial("tcp", sc.target, &ssh.ClientConfig{
		User: sc.user,
		// Timeout: TODO,
		// BannerCallback: TODO,
		HostKeyCallback: sc.hostKeyCallback,
		Auth: []ssh.AuthMethod{
			ssh.Password(sc.password),
			ssh.KeyboardInteractive(sc.keyboardInteractive),
		},
	})
	if err != nil {
		return err
	}

	var chchDone, rchDone bool
	for !chchDone && !rchDone {
		select {
		case nch := <-chch:
			if nch == nil {
				log.Infof("... No more channels")
				chch = nil
				chchDone = true
				continue
			}
			log.Infof("... New channel type %q extradata %v", nch.ChannelType(), nch.ExtraData())
			ch, req, err := nch.Accept()
			if err != nil {
				log.Errorf("Failed to accept channel: %v", err)
			}
			clientChannel, clientReq, err := sc.client.OpenChannel(nch.ChannelType(), nch.ExtraData())
			c := &channel{
				channel:       ch,
				clientChannel: clientChannel,
				clientReq:     clientReq,
				req:           req,
			}
			go func() {
				if err := c.run(ctx); err != nil {
					log.Errorf("Channel failed: %v", err)
				}
			}()
		case req := <-requestch:
			if req == nil {
				log.Infof("... No more requests")
				requestch = nil
				rchDone = true
				continue
			}
			log.Infof("... New connection req: %v", req)
		}
	}
	log.Infof("... Server connection closing")
	return nil
}
