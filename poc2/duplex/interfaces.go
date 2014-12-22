package duplex

import (
//"io"
)

type Channel interface {
	Write(data []byte) (int, error) // send
	Read(data []byte) (int, error)  // recv

	WriteFrame(data []byte) error // send_frame
	ReadFrame() ([]byte, error)   // recv_frame

	WriteError(data []byte) error // send_error
	ReadError() ([]byte, error)   // recv_error

	CloseWrite() error // send_end .. close_send?
	Close() error      // close

	//Open(chType, service string, headers []string) (Channel, error) // send_chan
	//Accept(chType string) (ChannelMeta, Channel)                    // recv_chan

	//Join(rwc io.ReadWriteCloser) // join(fd)
}

type ChannelMeta interface {
	Service() string
	Headers() []string
}

type peerConnection interface {
	Disconnect() error
	Name() string
	Endpoint() string
	Open(service string, headers []string) (Channel, error)
}

type peerListener interface {
	Unbind() error
}