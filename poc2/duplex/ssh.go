package duplex

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/user"
	"strings"

	"github.com/progrium/crypto/ssh"
)

// ssh keys

func loadPrivateKey(path string) (ssh.Signer, error) {
	if path[:2] == "~/" {
		usr, _ := user.Current()
		path = strings.Replace(path, "~", usr.HomeDir, 1)
	}
	pem, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(pem)
}

// ssh structs

type ssh_greetingPayload struct {
	Name string
}

type ssh_channelData struct {
	Service string
	Headers []string
}

// ssh listener

type ssh_peerListener struct {
	net.Listener
}

func (l *ssh_peerListener) Unbind() error {
	return l.Close()
}

// ssh connection

type ssh_peerConnection struct {
	addr string
	name string
	conn ssh.Conn
}

func (c *ssh_peerConnection) Disconnect() error {
	return c.conn.Close()
}

func (c *ssh_peerConnection) Name() string {
	return c.name
}

func (c *ssh_peerConnection) Addr() string {
	return c.addr
}

func (c *ssh_peerConnection) Open(service string, headers []string) (Channel, error) {
	meta := ssh_channelData{
		Service: service,
		Headers: headers,
	}
	ch, reqs, err := c.conn.OpenChannel("@duplex", ssh.Marshal(meta))
	if err != nil {
		return nil, err
	}
	go ssh.DiscardRequests(reqs)
	return &ssh_channel{ch, meta}, nil
}

// ssh server

func newPeerListener_ssh(peer *Peer, typ, addr string) (peerListener, error) {
	pk, err := loadPrivateKey(peer.GetOption(OptPrivateKey))
	if err != nil {
		return nil, err
	}
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), pk.PublicKey().Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unauthorized")
		},
	}
	config.AddHostKey(pk)

	if typ == "unix" {
		os.Remove(addr)
	}
	listener, err := net.Listen(typ, addr)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				peer.Unbind(typ + "://" + addr)
				return
			}
			go ssh_handleConn(conn, config, peer)
		}
	}()
	return &ssh_peerListener{listener}, nil
}

func ssh_handleConn(conn net.Conn, config *ssh.ServerConfig, peer *Peer) {
	defer conn.Close()
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		log.Println("debug: failed to handshake:", err)
		return
	}
	go ssh.DiscardRequests(reqs)
	peer.Lock()
	peer.conns[conn.RemoteAddr().String()] = &ssh_peerConnection{
		addr: conn.RemoteAddr().Network() + "://" + conn.RemoteAddr().String(),
		name: sshConn.User(),
		conn: sshConn,
	}
	peer.Unlock()
	ok, _, err := sshConn.SendRequest("@duplex-greeting", true,
		ssh.Marshal(&ssh_greetingPayload{peer.GetOption(OptName)}))
	if err != nil || !ok {
		log.Println("debug: failed to greet:", err)
		return
	}
	ssh_acceptChannels(chans, peer)
}

// ssh client

func newPeerConnection_ssh(peer *Peer, network, addr string) (peerConnection, error) {
	pk, err := loadPrivateKey(peer.GetOption(OptPrivateKey))
	if err != nil {
		return nil, err
	}
	config := &ssh.ClientConfig{
		User: peer.GetOption(OptName),
		Auth: []ssh.AuthMethod{ssh.PublicKeys(pk)},
	}
	netConn, err := net.Dial(network, addr)
	if err != nil {
		return nil, err
	}
	conn, chans, reqs, err := ssh.NewClientConn(netConn, addr, config)
	if err != nil {
		return nil, err
	}
	nameCh := make(chan string)
	go func() {
		for r := range reqs {
			switch r.Type {
			case "@duplex-greeting":
				var greeting ssh_greetingPayload
				err := ssh.Unmarshal(r.Payload, &greeting)
				if err != nil {
					continue
				}
				nameCh <- greeting.Name
				r.Reply(true, nil)
			default:
				// This handles keepalive messages and matches
				// the behaviour of OpenSSH.
				r.Reply(false, nil)
			}
		}
	}()
	name := <-nameCh // todo: timeout nameCh
	go ssh_acceptChannels(chans, peer)
	return &ssh_peerConnection{
		addr: network + "://" + addr,
		name: name,
		conn: conn,
	}, nil
}

// channels

type ssh_channel struct {
	ssh.Channel
	ssh_channelData
}

func (c *ssh_channel) ReadFrame() ([]byte, error) {
	bytes := make([]byte, 4)
	_, err := c.Read(bytes)
	if err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(bytes)
	frame := make([]byte, length)
	_, err = c.Read(frame)
	// handle errors based on written bytes
	if err != nil {
		return nil, err
	}
	return frame, nil
}

func (c *ssh_channel) WriteFrame(frame []byte) error {
	var buffer []byte
	n := uint32(len(frame))
	buffer = append(buffer, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	buffer = append(buffer, frame...)
	_, err := c.Write(buffer)
	return err
}

func (c *ssh_channel) Headers() []string {
	return c.ssh_channelData.Headers
}

func (c *ssh_channel) Service() string {
	return c.ssh_channelData.Service
}

func ssh_acceptChannels(chans <-chan ssh.NewChannel, peer *Peer) {
	var meta ssh_channelData
	for newCh := range chans {
		switch newCh.ChannelType() {
		case "@duplex":
			go func() {
				err := ssh.Unmarshal(newCh.ExtraData(), &meta)
				if err != nil {
					newCh.Reject(ssh.UnknownChannelType, "failed to parse channel data")
					return
				}
				if meta.Service == "" {
					newCh.Reject(ssh.UnknownChannelType, "empty service")
					return
				}
				ch, reqs, err := newCh.Accept()
				if err != nil {
					log.Println("debug: accept error:", err)
					return
				}
				go ssh.DiscardRequests(reqs)
				peer.incomingCh <- &ssh_channel{ch, meta}
			}()
		}
	}
}
