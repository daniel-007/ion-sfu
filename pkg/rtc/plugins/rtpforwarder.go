package plugins

import (
	"github.com/pion/rtp"

	"github.com/pion/ion-sfu/pkg/log"
	"github.com/pion/ion-sfu/pkg/rtc/transport"
)

// RTPForwarderConfig describes configuration parameters for the rtp forwarder.
type RTPForwarderConfig struct {
	On      bool   `mapstructure:"on"`
	Addr    string `mapstructure:"addr"`
	KcpKey  string `mapstructure:"kcpkey"`
	KcpSalt string `mapstructure:"kcpsalt"`
}

// RTPForwarder represents an RTPForwarder plugin.
// The RTPForwarder plugin forwards rtp packets using an RTPTransport
// to the configured endpoint. It can be used for sending raw stream rtp
// to another service for processing.
type RTPForwarder struct {
	id         string
	stop       bool
	Transport  *transport.RTPTransport
	outRTPChan chan *rtp.Packet
}

// NewRTPForwarder create new RTPForwarder. The RTPForwarder connects to
// the configured RTP endpoint.
func NewRTPForwarder(id, mid string, config RTPForwarderConfig) *RTPForwarder {
	log.Infof("New RTPForwarder Plugin with id %s address %s for mid %s", id, config.Addr, mid)
	var rtpTransport *transport.RTPTransport

	if config.KcpKey != "" && config.KcpSalt != "" {
		rtpTransport = transport.NewOutRTPTransportWithKCP(mid, config.Addr, config.KcpKey, config.KcpSalt)
	} else {
		rtpTransport = transport.NewOutRTPTransport(mid, config.Addr)
	}

	return &RTPForwarder{
		id:         id,
		Transport:  rtpTransport,
		outRTPChan: make(chan *rtp.Packet, maxSize),
	}
}

// ID returns the configured RTPForwarder ID.
func (r *RTPForwarder) ID() string {
	return r.id
}

// WriteRTP forwards rtp packet written to the RTPForwader.
func (r *RTPForwarder) WriteRTP(pkt *rtp.Packet) error {
	if r.stop {
		return nil
	}

	r.outRTPChan <- pkt
	go func() {
		err := r.Transport.WriteRTP(pkt)
		if err != nil {
			log.Errorf("r.Transport.WriteRTP => %s", err)
		}
	}()
	return nil
}

// ReadRTP can be used to read RTP packets written to the
// RTPForwader plugin after processing.
func (r *RTPForwarder) ReadRTP() <-chan *rtp.Packet {
	return r.outRTPChan
}

// Stop closes the rtp transport and halts forwarding.
func (r *RTPForwarder) Stop() {
	r.stop = true
	r.Transport.Close()
}
