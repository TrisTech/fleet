package fleet

import (
	"crypto/tls"
	"encoding/asn1"
	"encoding/gob"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TrisTech/goupd"
)

type Peer struct {
	c         *tls.Conn
	outStream [][]byte
	id        string
	name      string
	division  string
	addr      *net.TCPAddr
	valid     bool
	enc       *gob.Encoder

	write *sync.Mutex

	annIdx  uint64
	numG    uint32
	cnx     time.Time
	annTime time.Time
	Ping    time.Duration

	a *AgentObj

	mutex *sync.RWMutex
	unreg sync.Once

	alive chan interface{}
}

func (a *AgentObj) newConn(c net.Conn) {
	tc, ok := c.(*tls.Conn)
	if !ok {
		log.Printf("[fleet] non-tls connection recieved?")
		c.Close()
		return
	}

	// make sure handshake has completed
	err := tc.Handshake()
	if err != nil {
		log.Printf("[fleet] handshake failed with peer %s: %s", tc.RemoteAddr(), err)
		tc.Close()
		return
	}

	switch tc.ConnectionState().NegotiatedProtocol {
	case "fleet":
		a.handleFleetConn(tc)
		return
	case "p2p":
		a.handleServiceConn(tc)
		return
	default:
		tc.Close()
		log.Printf("[fleet] invalid protocol in connection handshake")
	}
}

func (a *AgentObj) handleFleetConn(tc *tls.Conn) {
	// instanciate peer and fetch certificate
	p := new(Peer)
	p.c = tc
	p.a = a
	p.cnx = time.Now()
	p.addr = tc.RemoteAddr().(*net.TCPAddr)
	p.write = &sync.Mutex{}
	p.mutex = &sync.RWMutex{}
	p.alive = make(chan interface{})
	p.enc = gob.NewEncoder(p.c)
	err := p.fetchUuidFromCertificate()
	if err != nil {
		log.Printf("[fleet] failed to get peer id: %s", err)
		p.c.Close()
		return
	}
	if p.id == a.id {
		log.Printf("[fleet] connected to self, closing")
		p.c.Close()
		return
	}

	log.Printf("[fleet] Connection with peer %s established", p.id)

	p.valid = true
	p.register()
	p.sendHandshake()
	go p.loop()
	go p.monitor()
}

func (p *Peer) retryLater(t time.Duration) {
	time.Sleep(t)
	p.a.dialPeer(p.addr.IP.String(), p.name, p.id)
}

func (p *Peer) loop() {
	// read from peer
	dec := gob.NewDecoder(p.c)
	var pkt Packet

	for {
		err := dec.Decode(&pkt)
		if err != nil {
			if err == io.EOF {
				log.Printf("[fleet] disconnected peer %s", p.id)
			} else {
				log.Printf("[fleet] failed to read from peer %s: %s", p.id, err)
			}

			if p.valid {
				go p.retryLater(10 * time.Second)
			}

			p.c.Close()
			p.unregister()

			return
		}

		p.handlePacket(pkt)
	}
}

func (p *Peer) monitor() {
	t := time.NewTicker(5 * time.Second)

	for {
		select {
		case <-p.alive:
			p.c.Close()
			return
		case <-t.C:
			if time.Since(p.annTime) > time.Minute {
				p.c.Close()
				p.unregister()
				return
			}
		}
	}
}

func (p *Peer) handlePacket(pktI interface{}) error {
	switch pkt := pktI.(type) {
	case *PacketHandshake:
		if pkt.Id != p.id {
			return errors.New("invalid handshake")
		}
		p.name = pkt.Name
		p.division = pkt.Division
		goupd.SignalVersion(pkt.Git, pkt.Build)
		// TODO calculate offset
		return nil
	case *PacketSeed:
		return handleNewSeed(pkt.Seed, pkt.Time)
	case *PacketAnnounce:
		return p.a.handleAnnounce(pkt, p)
	case *PacketPong:
		if pkt.TargetId != p.a.id {
			// forward
			return p.a.SendTo(pkt.TargetId, pkt)
		}
		sp := p.a.GetPeer(pkt.SourceId)
		if sp != nil {
			sp.handlePong(pkt)
		}
		return nil
	case *PacketRpc:
		if pkt.TargetId != p.a.id {
			// fw
			return p.a.SendTo(pkt.TargetId, pkt)
		}
		// we don't really care about the source, just do the rpc thing
		return p.a.handleRpc(pkt)
	case *PacketDbRecord:
		if pkt.TargetId != p.a.id {
			// fw
			return p.a.SendTo(pkt.TargetId, pkt)
		}
		// let the db handle that
		return feedDbSet(pkt.Bucket, pkt.Key, pkt.Val, pkt.Stamp)
	default:
		return errors.New("unsupported packet")
	}
}

func (p *Peer) processAnnounce(ann *PacketAnnounce, fromPeer *Peer) error {
	if ann.Idx <= p.annIdx {
		return nil
	}

	p.mutex.Lock()
	defer p.mutex.Unlock()

	p.annIdx = ann.Idx
	p.annTime = ann.Now
	atomic.StoreUint32(&p.numG, ann.NumG)

	// send response
	p.a.SendTo(ann.Id, &PacketPong{TargetId: ann.Id, SourceId: p.a.id, Now: ann.Now})

	// broadcast
	p.a.doBroadcast(ann, fromPeer.id)

	return nil
}

func (p *Peer) handlePong(pong *PacketPong) {
	// store pong info
	p.Ping = time.Since(pong.Now)
}

func (p *Peer) fetchUuidFromCertificate() error {
	// grab certificate
	chains := p.c.ConnectionState().VerifiedChains
	if len(chains) == 0 {
		return errors.New("no peer certificate?")
	}
	if len(chains[0]) == 0 {
		return errors.New("no peer certificate? (2)")
	}

	peer_cert := chains[0][0] // *x509.Certificate
	// grab id
	peer_subject := peer_cert.Subject
	peer_id := ""
	for _, name := range peer_subject.Names {
		// oid(2.5.4.45) = UniqueIdentifier
		if !name.Type.Equal(asn1.ObjectIdentifier{2, 5, 4, 45}) {
			continue
		}

		peer_id = name.Value.(string)
		break
	}

	if peer_id == "" {
		return errors.New("failed to get peer id from cert")
	}

	p.id = peer_id
	return nil
}

func (p *Peer) Send(pkt Packet) error {
	// use mutex here to avoid multiple writes to overlap
	p.write.Lock()
	defer p.write.Unlock()

	err := p.enc.Encode(&pkt)
	if err != nil {
		log.Printf("[fleet] Write to peer failed: %s", err)
		p.c.Close()
	}
	return nil
}

func (p *Peer) register() {
	a := p.a
	a.peersMutex.Lock()
	defer a.peersMutex.Unlock()

	old, ok := a.peers[p.id]
	if ok {
		old.c.Close()
	}

	a.peers[p.id] = p
}

func (p *Peer) unregister() {
	p.unreg.Do(func() {
		a := p.a

		a.peersMutex.Lock()
		defer a.peersMutex.Unlock()

		old, ok := a.peers[p.id]
		if ok && old == p {
			delete(a.peers, p.id)
		}

		close(p.alive) // no more alive
	})
}

func (p *Peer) sendHandshake() error {
	pkt := &PacketHandshake{
		Id:       p.a.id,
		Name:     p.a.name,
		Division: p.a.division,
		Now:      time.Now(),
		Git:      goupd.GIT_TAG,
		Build:    goupd.DATE_TAG,
	}
	err := p.Send(pkt)
	if err != nil {
		return err
	}
	return p.Send(seedPacket())
}
