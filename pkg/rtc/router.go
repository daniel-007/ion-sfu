package rtc

import (
	"math"
	"sync"
	"time"

	"github.com/pion/ion-sfu/pkg/log"
	"github.com/pion/ion-sfu/pkg/rtc/plugins"
	"github.com/pion/ion-sfu/pkg/rtc/transport"
	"github.com/pion/ion-sfu/pkg/util"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

const (
	maxWriteErr = 100
)

type RouterConfig struct {
	MinBandwidth uint64 `mapstructure:"minbandwidth"`
	MaxBandwidth uint64 `mapstructure:"maxbandwidth"`
	REMBFeedback bool   `mapstructure:"rembfeedback"`
}

//                                      +--->sub
//                                      |
// pub--->pubCh-->pluginChain-->subCh---+--->sub
//                                      |
//                                      +--->sub
// Router is rtp router
type Router struct {
	id             string
	pub            transport.Transport
	subs           map[string]transport.Transport
	subLock        sync.RWMutex
	stop           bool
	pluginChain    *plugins.PluginChain
	subChans       map[string]chan *rtp.Packet
	rembChan       chan *rtcp.ReceiverEstimatedMaximumBitrate
	onCloseHandler func()
}

// NewRouter return a new Router
func NewRouter(id string) *Router {
	log.Infof("NewRouter id=%s", id)
	return &Router{
		id:          id,
		subs:        make(map[string]transport.Transport),
		pluginChain: plugins.NewPluginChain(id),
		subChans:    make(map[string]chan *rtp.Packet),
		rembChan:    make(chan *rtcp.ReceiverEstimatedMaximumBitrate),
	}
}

// InitPlugins initializes plugins for the router
func (r *Router) InitPlugins(config plugins.Config) error {
	log.Infof("Router.InitPlugins config=%+v", config)
	if r.pluginChain != nil {
		return r.pluginChain.Init(config)
	}
	return nil
}

func (r *Router) start() {
	if routerConfig.REMBFeedback {
		go r.rembLoop()
	}
	go func() {
		defer util.Recover("[Router.start]")
		for {
			if r.stop {
				return
			}

			var pkt *rtp.Packet
			var err error
			// get rtp from pluginChain or pub
			if r.pluginChain != nil && r.pluginChain.On() {
				pkt = r.pluginChain.ReadRTP()
			} else {
				pkt, err = r.pub.ReadRTP()
				if err != nil {
					log.Errorf("r.pub.ReadRTP err=%v", err)
					continue
				}
			}
			// log.Debugf("pkt := <-r.subCh %v", pkt)
			if pkt == nil {
				continue
			}
			r.subLock.RLock()
			// Push to client send queues
			for i := range r.GetSubs() {
				// Nonblock sending
				select {
				case r.subChans[i] <- pkt:
				default:
					log.Errorf("Sub consumer is backed up. Dropping packet")
				}
			}
			r.subLock.RUnlock()
		}
	}()
}

// AddPub add a pub transport to the router
func (r *Router) AddPub(t transport.Transport) transport.Transport {
	log.Infof("AddPub")
	r.pub = t
	r.pluginChain.AttachPub(t)
	r.start()
	t.OnClose(func() {
		r.Close()
	})
	return t
}

// delPub
func (r *Router) delPub() {
	log.Infof("Router.delPub %s", r.pub.ID())
	if r.pub != nil {
		r.pub.Close()
	}
	if r.pluginChain != nil {
		r.pluginChain.Close()
	}
	r.pub = nil
}

// GetPub get pub
func (r *Router) GetPub() transport.Transport {
	// log.Infof("Router.GetPub %v", r.pub)
	return r.pub
}

func (r *Router) subWriteLoop(subID string, trans transport.Transport) {
	for pkt := range r.subChans[subID] {
		// log.Infof(" WriteRTP %v:%v to %v PT: %v", pkt.SSRC, pkt.SequenceNumber, trans.ID(), pkt.Header.PayloadType)

		if err := trans.WriteRTP(pkt); err != nil {
			// log.Errorf("wt.WriteRTP err=%v", err)
			// del sub when err is increasing
			if trans.WriteErrTotal() > maxWriteErr {
				r.delSub(trans.ID())
			}
		}
		trans.WriteErrReset()
	}
	log.Infof("Closing sub writer")
}

func (r *Router) rembLoop() {
	lastRembTime := time.Now()
	maxRembTime := 200 * time.Millisecond
	rembMin := routerConfig.MinBandwidth
	rembMax := routerConfig.MaxBandwidth
	if rembMin == 0 {
		rembMin = 10000 //10 KBit
	}
	if rembMax == 0 {
		rembMax = 100000000 //100 MBit
	}
	var lowest uint64 = math.MaxUint64
	var rembCount, rembTotalRate uint64

	for pkt := range r.rembChan {
		// Update stats
		rembCount++
		rembTotalRate += pkt.Bitrate
		if pkt.Bitrate < lowest {
			lowest = pkt.Bitrate
		}

		// Send upstream if time
		if time.Since(lastRembTime) > maxRembTime {
			lastRembTime = time.Now()
			avg := uint64(rembTotalRate / rembCount)

			_ = avg
			target := lowest

			if target < rembMin {
				target = rembMin
			} else if target > rembMax {
				target = rembMax
			}

			newPkt := &rtcp.ReceiverEstimatedMaximumBitrate{
				Bitrate:    target,
				SenderSSRC: 1,
				SSRCs:      pkt.SSRCs,
			}

			log.Infof("Router.rembLoop send REMB: %+v", newPkt)

			if r.GetPub() != nil {
				err := r.GetPub().WriteRTCP(newPkt)
				if err != nil {
					log.Errorf("Router.rembLoop err => %+v", err)
				}
			}

			// Reset stats
			rembCount = 0
			rembTotalRate = 0
			lowest = math.MaxUint64
		}
	}
}

func (r *Router) subFeedbackLoop(subID string, trans transport.Transport) {
	for pkt := range trans.GetRTCPChan() {
		if r.stop {
			break
		}
		switch pkt := pkt.(type) {
		case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
			if r.GetPub() != nil {
				// Request a Key Frame
				log.Infof("Router got pli: %d", pkt.DestinationSSRC())
				err := r.GetPub().WriteRTCP(pkt)
				if err != nil {
					log.Errorf("Router pli err => %+v", err)
				}
			}
		case *rtcp.ReceiverEstimatedMaximumBitrate:
			if routerConfig.REMBFeedback {
				r.rembChan <- pkt
			}
		case *rtcp.TransportLayerNack:
			// log.Infof("Router got nack: %+v", pkt)
			nack := pkt
			for _, nackPair := range nack.Nacks {
				if !r.resendRTP(subID, nack.MediaSSRC, nackPair.PacketID) {
					n := &rtcp.TransportLayerNack{
						//origin ssrc
						SenderSSRC: nack.SenderSSRC,
						MediaSSRC:  nack.MediaSSRC,
						Nacks:      []rtcp.NackPair{{PacketID: nackPair.PacketID}},
					}
					if r.pub != nil {
						err := r.GetPub().WriteRTCP(n)
						if err != nil {
							log.Errorf("Router nack WriteRTCP err => %+v", err)
						}
					}
				}
			}

		default:
		}
	}
	log.Infof("Closing sub feedback")
}

// AddSub add a sub to router
func (r *Router) AddSub(id string, t transport.Transport) transport.Transport {
	//fix panic: assignment to entry in nil map
	if r.stop {
		return nil
	}
	r.subLock.Lock()
	defer r.subLock.Unlock()
	r.subs[id] = t
	r.subChans[id] = make(chan *rtp.Packet, 1000)
	log.Infof("Router.AddSub id=%s t=%p", id, t)

	t.OnClose(func() {
		r.delSub(id)
	})

	// Sub loops
	go r.subWriteLoop(id, t)
	go r.subFeedbackLoop(id, t)
	return t
}

// GetSub get a sub by id
func (r *Router) GetSub(id string) transport.Transport {
	r.subLock.RLock()
	defer r.subLock.RUnlock()
	// log.Infof("Router.GetSub id=%s sub=%v", id, r.subs[id])
	return r.subs[id]
}

// GetSubs get all subs
func (r *Router) GetSubs() map[string]transport.Transport {
	r.subLock.RLock()
	defer r.subLock.RUnlock()
	// log.Infof("Router.GetSubs len=%v", len(r.subs))
	return r.subs
}

// delSub del sub by id
func (r *Router) delSub(id string) {
	log.Infof("Router.delSub id=%s", id)
	r.subLock.Lock()
	defer r.subLock.Unlock()
	if r.subs[id] != nil {
		r.subs[id].Close()
	}
	if r.subChans[id] != nil {
		close(r.subChans[id])
	}
	delete(r.subs, id)
	delete(r.subChans, id)
}

// delSubs del all sub
func (r *Router) delSubs() {
	log.Infof("Router.delSubs")
	r.subLock.RLock()
	keys := make([]string, 0, len(r.subs))
	for k := range r.subs {
		keys = append(keys, k)
	}
	r.subLock.RUnlock()

	for _, id := range keys {
		r.delSub(id)
	}
}

// Close release all
func (r *Router) Close() {
	if r.stop {
		return
	}
	log.Infof("Router.Close")
	r.onCloseHandler()
	r.delPub()
	r.stop = true
	r.delSubs()
}

// OnClose handler called when router is closed.
func (r *Router) OnClose(f func()) {
	r.onCloseHandler = f
}

func (r *Router) resendRTP(sid string, ssrc uint32, sn uint16) bool {
	if r.pub == nil {
		return false
	}
	hd := r.pluginChain.GetPlugin(plugins.TypeJitterBuffer)
	if hd != nil {
		jb := hd.(*plugins.JitterBuffer)
		pkt := jb.GetPacket(ssrc, sn)
		if pkt == nil {
			// log.Infof("Router.resendRTP pkt not found sid=%s ssrc=%d sn=%d pkt=%v", sid, ssrc, sn, pkt)
			return false
		}
		sub := r.GetSub(sid)
		if sub != nil {
			err := sub.WriteRTP(pkt)
			if err != nil {
				log.Errorf("router.resendRTP err=%v", err)
			}
			// log.Infof("Router.resendRTP sid=%s ssrc=%d sn=%d", sid, ssrc, sn)
			return true
		}
	}
	return false
}
