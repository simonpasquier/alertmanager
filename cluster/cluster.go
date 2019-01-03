// Copyright 2018 Prometheus Team
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cluster

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/hashicorp/memberlist"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/discovery"
	"github.com/prometheus/prometheus/discovery/dns"
	"github.com/prometheus/prometheus/discovery/targetgroup"
)

// Peer is a single peer in a gossip cluster.
type Peer struct {
	mlist    *memberlist.Memberlist
	delegate *delegate

	mtx    sync.RWMutex
	states map[string]State
	stopc  chan struct{}
	readyc chan struct{}

	peerLock    sync.RWMutex
	peers       map[string]peer
	failedPeers []peer

	knownPeers []string
	peerc      chan []string

	advertiseAddr string

	// SD stuff
	sd       *discovery.Manager
	sdCancel context.CancelFunc

	failedReconnectionsCounter prometheus.Counter
	reconnectionsCounter       prometheus.Counter
	failedRefreshCounter       prometheus.Counter
	refreshCounter             prometheus.Counter
	peerLeaveCounter           prometheus.Counter
	peerUpdateCounter          prometheus.Counter
	peerJoinCounter            prometheus.Counter

	logger log.Logger
}

// peer is an internal type used for bookkeeping. It holds the state of peers
// in the cluster.
type peer struct {
	status    PeerStatus
	leaveTime time.Time

	*memberlist.Node
}

// PeerStatus is the state that a peer is in.
type PeerStatus int

const (
	StatusNone PeerStatus = iota
	StatusAlive
	StatusFailed
)

func (s PeerStatus) String() string {
	switch s {
	case StatusNone:
		return "none"
	case StatusAlive:
		return "alive"
	case StatusFailed:
		return "failed"
	default:
		panic(fmt.Sprintf("unknown PeerStatus: %d", s))
	}
}

const (
	DefaultPushPullInterval  = 60 * time.Second
	DefaultGossipInterval    = 200 * time.Millisecond
	DefaultTcpTimeout        = 10 * time.Second
	DefaultProbeTimeout      = 500 * time.Millisecond
	DefaultProbeInterval     = 1 * time.Second
	DefaultReconnectInterval = 10 * time.Second
	DefaultReconnectTimeout  = 6 * time.Hour
	DefaultRefreshInterval   = 15 * time.Second
	maxGossipPacketSize      = 1400
)

func Create(
	l log.Logger,
	reg prometheus.Registerer,
	bindAddr string,
	advertiseAddr string,
	knownPeers []string,
	waitIfEmpty bool,
	pushPullInterval time.Duration,
	gossipInterval time.Duration,
	tcpTimeout time.Duration,
	probeTimeout time.Duration,
	probeInterval time.Duration,
) (*Peer, error) {
	bindHost, bindPortStr, err := net.SplitHostPort(bindAddr)
	if err != nil {
		return nil, err
	}
	bindPort, err := strconv.Atoi(bindPortStr)
	if err != nil {
		return nil, errors.Wrap(err, "invalid listen address")
	}

	// Verify that the provided address to advertise is valid.
	var advertiseHost string
	var advertisePort int
	if advertiseAddr != "" {
		var advertisePortStr string
		advertiseHost, advertisePortStr, err = net.SplitHostPort(advertiseAddr)
		if err != nil {
			return nil, errors.Wrap(err, "invalid advertise address")
		}
		advertisePort, err = strconv.Atoi(advertisePortStr)
		if err != nil {
			return nil, errors.Wrap(err, "invalid advertise address, wrong port")
		}
	}

	// Initial validation of the user-specified advertise address.
	addr, err := calculateAdvertiseAddress(bindHost, advertiseHost)
	if err != nil {
		level.Warn(l).Log("err", "couldn't deduce an advertise address: "+err.Error())
	} else if isAny(bindAddr) && advertiseHost == "" {
		// memberlist doesn't advertise properly when the bind address is empty or unspecified.
		level.Info(l).Log("msg", "setting advertise address explicitly", "addr", addr.String(), "port", bindPort)
		advertiseHost = addr.String()
		advertisePort = bindPort
	}

	// TODO(fabxc): generate human-readable but random names?
	name, err := ulid.New(ulid.Now(), rand.New(rand.NewSource(time.Now().UnixNano())))
	if err != nil {
		return nil, err
	}

	// Setup the service discovery.
	ctx, cancel := context.WithCancel(context.Background())
	sd := discovery.NewManager(ctx, l, discovery.Name("cluster"), discovery.WaitDuration(1*time.Millisecond))
	providers := make([]discovery.Provider, 0)
	for _, addr := range knownPeers {
		providers = append(providers, newProviders(addr, l)...)
	}
	err = sd.ApplyConfig(providers)
	if err != nil {
		return nil, err
	}

	p := &Peer{
		states:     map[string]State{},
		stopc:      make(chan struct{}),
		readyc:     make(chan struct{}),
		peerc:      make(chan []string),
		logger:     l,
		peers:      map[string]peer{},
		knownPeers: make([]string, 0),

		sd:       sd,
		sdCancel: cancel,
	}

	p.register(reg)

	retransmit := len(knownPeers) / 2
	if retransmit < 3 {
		retransmit = 3
	}
	p.delegate = newDelegate(l, reg, p, retransmit)

	cfg := memberlist.DefaultLANConfig()
	cfg.Name = name.String()
	cfg.BindAddr = bindHost
	cfg.BindPort = bindPort
	cfg.Delegate = p.delegate
	cfg.Events = p.delegate
	cfg.GossipInterval = gossipInterval
	cfg.PushPullInterval = pushPullInterval
	cfg.TCPTimeout = tcpTimeout
	cfg.ProbeTimeout = probeTimeout
	cfg.ProbeInterval = probeInterval
	cfg.LogOutput = &logWriter{l: l}
	cfg.GossipNodes = retransmit
	cfg.UDPBufferSize = maxGossipPacketSize

	if advertiseHost != "" {
		cfg.AdvertiseAddr = advertiseHost
		cfg.AdvertisePort = advertisePort
	}

	ml, err := memberlist.Create(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "create memberlist")
	}
	p.mlist = ml
	p.advertiseAddr = ml.LocalNode().Address()

	go p.runSD(ctx)

	return p, nil
}

func (p *Peer) runSD(ctx context.Context) {
	go func() {
		p.sd.Run()
		level.Info(p.logger).Log("msg", "cluster service discovery stopped")
	}()

	for {
		select {
		case tsets := <-p.sd.SyncCh():
			level.Debug(p.logger).Log("msg", "received updates from the service discovery")
			peers := make([]string, 0)
			present := make(map[string]struct{})
			for _, tset := range tsets {
				for _, tg := range tset {
					for _, t := range tg.Targets {
						addr := string(t[model.AddressLabel])
						if addr == p.advertiseAddr {
							// Don't include ourselves.
							continue
						}
						if _, ok := present[addr]; ok {
							continue
						}
						peers = append(peers, addr)
					}
				}
			}

			select {
			case p.peerc <- peers:
			case <-ctx.Done():
				return
			}

		case <-ctx.Done():
			return
		}
	}

}

// newProviders returns a list of SD providers from a given address.
func newProviders(addr string, l log.Logger) []discovery.Provider {
	const refresh = model.Duration(time.Minute)
	var cfgs []dns.SDConfig

	host, port, err := net.SplitHostPort(addr)
	if err == nil {
		ip := net.ParseIP(host)
		if ip != nil {
			// This is already an IP address.
			return []discovery.Provider{
				discovery.NewProvider(addr, staticAddress(addr)),
			}
		}
		portInt, err := strconv.Atoi(port)
		if err != nil {
			level.Warn(l).Log("msg", "invalid address port", "err", err)
			return nil
		}
		cfgs = []dns.SDConfig{
			{
				RefreshInterval: refresh,
				Type:            "A",
				Names:           []string{addr},
				Port:            portInt,
			},
			{
				RefreshInterval: refresh,
				Type:            "AAAA",
				Names:           []string{addr},
				Port:            portInt,
			}}
	} else {
		// Assume that it is a SRV record.
		cfgs = []dns.SDConfig{
			{
				RefreshInterval: refresh,
				Type:            "SRV",
				Names:           []string{addr},
			},
		}
	}

	providers := make([]discovery.Provider, len(cfgs))
	for i, cfg := range cfgs {
		d := dns.NewDiscovery(cfg, l)
		providers = append(providers, discovery.NewProvider(fmt.Sprintf("%s/%d", addr, i), d))

	}
	return providers
}

// Start initiates the process to join the cluster.
func (p *Peer) Start(reconnectInterval time.Duration, reconnectTimeout time.Duration) {
	if reconnectInterval != 0 {
		go p.handleReconnect(reconnectInterval)
	}
	if reconnectTimeout != 0 {
		go p.handleReconnectTimeout(5*time.Minute, reconnectTimeout)
	}
	go p.handleRefresh(DefaultRefreshInterval)
}

// handleRefresh reconnects with known peers either when they are discovered or when the connection is lost.
func (p *Peer) handleRefresh(d time.Duration) {
	for {
		t := time.NewTimer(d)
		select {
		case <-p.stopc:
			return
		case peers := <-p.peerc:
			if !t.Stop() {
				<-t.C
			}
			level.Debug(p.logger).Log("msg", "received updated peers list", "peers", strings.Join(peers, ","))
			if hasNonlocal(peers) && isUnroutable(p.advertiseAddr) {
				level.Warn(p.logger).Log("err", "this node advertises itself on an unroutable address", "addr", p.advertiseAddr)
				level.Warn(p.logger).Log("err", "this node will be unreachable in the cluster")
				level.Warn(p.logger).Log("err", "provide --cluster.advertise-address as a routable IP address or hostname")
			}
			// TODO: clean up peers that were known before but are now gone?
			p.knownPeers = peers
			p.refresh()
		case <-t.C:
			p.refresh()
		}
	}
}

func (p *Peer) refresh() {
	logger := log.With(p.logger, "msg", "refresh known peers")

	members := p.mlist.Members()
	for _, peer := range p.knownPeers {
		var isPeerFound bool
		for _, member := range members {
			if member.Address() == peer {
				isPeerFound = true
				break
			}
		}

		if !isPeerFound {
			if _, err := p.mlist.Join([]string{peer}); err != nil {
				p.failedRefreshCounter.Inc()
				level.Warn(logger).Log("result", "failure", "addr", peer)
			} else {
				p.refreshCounter.Inc()
				level.Debug(logger).Log("result", "success", "addr", peer)
			}
		}
	}
}

type logWriter struct {
	l log.Logger
}

func (l *logWriter) Write(b []byte) (int, error) {
	return len(b), level.Debug(l.l).Log("memberlist", string(b))
}

func (p *Peer) register(reg prometheus.Registerer) {
	clusterFailedPeers := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "alertmanager_cluster_failed_peers",
		Help: "Number indicating the current number of failed peers in the cluster.",
	}, func() float64 {
		p.peerLock.RLock()
		defer p.peerLock.RUnlock()

		return float64(len(p.failedPeers))
	})
	p.failedReconnectionsCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "alertmanager_cluster_reconnections_failed_total",
		Help: "A counter of the number of failed cluster peer reconnection attempts.",
	})

	p.reconnectionsCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "alertmanager_cluster_reconnections_total",
		Help: "A counter of the number of cluster peer reconnections.",
	})

	p.failedRefreshCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "alertmanager_cluster_refresh_join_failed_total",
		Help: "A counter of the number of failed cluster peer joined attempts via refresh.",
	})
	p.refreshCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "alertmanager_cluster_refresh_join_total",
		Help: "A counter of the number of cluster peer joined via refresh.",
	})

	p.peerLeaveCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "alertmanager_cluster_peers_left_total",
		Help: "A counter of the number of peers that have left.",
	})
	p.peerUpdateCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "alertmanager_cluster_peers_update_total",
		Help: "A counter of the number of peers that have updated metadata.",
	})
	p.peerJoinCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "alertmanager_cluster_peers_joined_total",
		Help: "A counter of the number of peers that have joined.",
	})

	reg.MustRegister(clusterFailedPeers, p.failedReconnectionsCounter, p.reconnectionsCounter,
		p.peerLeaveCounter, p.peerUpdateCounter, p.peerJoinCounter, p.refreshCounter, p.failedRefreshCounter)
}

// handleReconnectTimeout periodically cleans the list of disconnected peers.
func (p *Peer) handleReconnectTimeout(d time.Duration, timeout time.Duration) {
	tick := time.NewTicker(d)
	defer tick.Stop()

	for {
		select {
		case <-p.stopc:
			return
		case <-tick.C:
			p.removeFailedPeers(timeout)
		}
	}
}

// removeFailedPeers forgets about peers that haven't been seen for the given duration.
func (p *Peer) removeFailedPeers(timeout time.Duration) {
	p.peerLock.Lock()
	defer p.peerLock.Unlock()

	now := time.Now()

	keep := make([]peer, 0, len(p.failedPeers))
	for _, pr := range p.failedPeers {
		if pr.leaveTime.Add(timeout).After(now) {
			keep = append(keep, pr)
		} else {
			level.Debug(p.logger).Log("msg", "failed peer has timed out", "peer", pr.Node, "addr", pr.Address())
			delete(p.peers, pr.Name)
		}
	}

	p.failedPeers = keep
}

// handleReconnect periodically tries to reconnect to lost peers.
func (p *Peer) handleReconnect(d time.Duration) {
	tick := time.NewTicker(d)
	defer tick.Stop()

	for {
		select {
		case <-p.stopc:
			return
		case <-tick.C:
			p.reconnect()
		}
	}
}

// reconnect tries to reconnect to lost peers.
func (p *Peer) reconnect() {
	p.peerLock.RLock()
	failedPeers := p.failedPeers
	p.peerLock.RUnlock()

	logger := log.With(p.logger, "msg", "reconnect")
	for _, pr := range failedPeers {
		// No need to do book keeping on failedPeers here. If a
		// reconnect is successful, they will be announced in
		// peerJoin().
		if _, err := p.mlist.Join([]string{pr.Address()}); err != nil {
			p.failedReconnectionsCounter.Inc()
			level.Debug(logger).Log("result", "failure", "peer", pr.Node, "addr", pr.Address())
		} else {
			p.reconnectionsCounter.Inc()
			level.Debug(logger).Log("result", "success", "peer", pr.Node, "addr", pr.Address())
		}
	}
}

// peerJoin is called by the memberlist library whenever a new peer connects.
func (p *Peer) peerJoin(n *memberlist.Node) {
	p.peerLock.Lock()
	defer p.peerLock.Unlock()

	var oldStatus PeerStatus
	pr, ok := p.peers[n.Address()]
	if !ok {
		oldStatus = StatusNone
		pr = peer{
			status: StatusAlive,
			Node:   n,
		}
	} else {
		oldStatus = pr.status
		pr.Node = n
		pr.status = StatusAlive
		pr.leaveTime = time.Time{}
	}

	p.peers[n.Address()] = pr
	p.peerJoinCounter.Inc()

	if oldStatus == StatusFailed {
		level.Debug(p.logger).Log("msg", "peer rejoined", "peer", pr.Node)
		p.failedPeers = removeOldPeer(p.failedPeers, pr.Address())
	}
}

// peerLeave is called by the memberlist library whenever a peer disconnects.
func (p *Peer) peerLeave(n *memberlist.Node) {
	p.peerLock.Lock()
	defer p.peerLock.Unlock()

	pr, ok := p.peers[n.Address()]
	if !ok {
		// Why are we receiving a leave notification from a node that
		// never joined?
		return
	}

	pr.status = StatusFailed
	pr.leaveTime = time.Now()
	p.failedPeers = append(p.failedPeers, pr)
	p.peers[n.Address()] = pr

	p.peerLeaveCounter.Inc()
	level.Debug(p.logger).Log("msg", "peer left", "peer", pr.Node)
}

// peerUpdate is called by the memberlist library whenever a peer updates its metadata.
func (p *Peer) peerUpdate(n *memberlist.Node) {
	p.peerLock.Lock()
	defer p.peerLock.Unlock()

	pr, ok := p.peers[n.Address()]
	if !ok {
		// Why are we receiving an update from a node that never
		// joined?
		return
	}

	pr.Node = n
	p.peers[n.Address()] = pr

	p.peerUpdateCounter.Inc()
	level.Debug(p.logger).Log("msg", "peer updated", "peer", pr.Node)
}

// AddState adds a new state that will be gossiped. It returns a channel to which
// broadcast messages for the state can be sent.
func (p *Peer) AddState(key string, s State, reg prometheus.Registerer) *Channel {
	p.states[key] = s
	send := func(b []byte) {
		p.delegate.bcast.QueueBroadcast(simpleBroadcast(b))
	}
	peers := func() []*memberlist.Node {
		nodes := p.Peers()
		for i, n := range nodes {
			if n.Name == p.Self().Name {
				nodes = append(nodes[:i], nodes[i+1:]...)
				break
			}
		}
		return nodes
	}
	sendOversize := func(n *memberlist.Node, b []byte) error {
		return p.mlist.SendReliable(n, b)
	}
	return NewChannel(key, send, peers, sendOversize, p.logger, p.stopc, reg)
}

// Leave the cluster, waiting up to timeout.
func (p *Peer) Stop(timeout time.Duration) error {
	close(p.stopc)
	p.sdCancel()
	level.Debug(p.logger).Log("msg", "leaving cluster")
	return p.mlist.Leave(timeout)
}

// Name returns the unique ID of this peer in the cluster.
func (p *Peer) Name() string {
	return p.mlist.LocalNode().Name
}

// ClusterSize returns the current number of alive members in the cluster.
func (p *Peer) ClusterSize() int {
	return p.mlist.NumMembers()
}

// Return true when router has settled.
func (p *Peer) Ready() bool {
	select {
	case <-p.readyc:
		return true
	default:
	}
	return false
}

// Wait until Settle() has finished.
func (p *Peer) WaitReady() {
	<-p.readyc
}

// Return a status string representing the peer state.
func (p *Peer) Status() string {
	if p.Ready() {
		return "ready"
	} else {
		return "settling"
	}
}

// Info returns a JSON-serializable dump of cluster state.
// Useful for debug.
func (p *Peer) Info() map[string]interface{} {
	p.mtx.RLock()
	defer p.mtx.RUnlock()

	return map[string]interface{}{
		"self":    p.mlist.LocalNode(),
		"members": p.mlist.Members(),
	}
}

// Self returns the node information about the peer itself.
func (p *Peer) Self() *memberlist.Node {
	return p.mlist.LocalNode()
}

// Peers returns the peers in the cluster.
func (p *Peer) Peers() []*memberlist.Node {
	return p.mlist.Members()
}

// Position returns the position of the peer in the cluster.
func (p *Peer) Position() int {
	all := p.Peers()
	sort.Slice(all, func(i, j int) bool {
		return all[i].Name < all[j].Name
	})

	k := 0
	for _, n := range all {
		if n.Name == p.Self().Name {
			break
		}
		k++
	}
	return k
}

// Settle waits until the mesh is ready (and sets the appropriate internal state when it is).
// The idea is that we don't want to start "working" before we get a chance to know most of the alerts and/or silences.
// Inspired from https://github.com/apache/cassandra/blob/7a40abb6a5108688fb1b10c375bb751cbb782ea4/src/java/org/apache/cassandra/gms/Gossiper.java
// This is clearly not perfect or strictly correct but should prevent the alertmanager to send notification before it is obviously not ready.
// This is especially important for those that do not have persistent storage.
func (p *Peer) Settle(ctx context.Context, interval time.Duration) {
	const NumOkayRequired = 3
	level.Info(p.logger).Log("msg", "Waiting for gossip to settle...", "interval", interval)
	start := time.Now()
	nPeers := 0
	nOkay := 0
	totalPolls := 0
	for {
		select {
		case <-ctx.Done():
			elapsed := time.Since(start)
			level.Info(p.logger).Log("msg", "gossip not settled but continuing anyway", "polls", totalPolls, "elapsed", elapsed)
			close(p.readyc)
			return
		case <-time.After(interval):
		}
		elapsed := time.Since(start)
		n := len(p.Peers())
		if nOkay >= NumOkayRequired {
			level.Info(p.logger).Log("msg", "gossip settled; proceeding", "elapsed", elapsed)
			break
		}
		if n == nPeers {
			nOkay++
			level.Debug(p.logger).Log("msg", "gossip looks settled", "elapsed", elapsed)
		} else {
			nOkay = 0
			level.Info(p.logger).Log("msg", "gossip not settled", "polls", totalPolls, "before", nPeers, "now", n, "elapsed", elapsed)
		}
		nPeers = n
		totalPolls++
	}
	close(p.readyc)
}

// State is a piece of state that can be serialized and merged with other
// serialized state.
type State interface {
	// MarshalBinary serializes the underlying state.
	MarshalBinary() ([]byte, error)

	// Merge merges serialized state into the underlying state.
	Merge(b []byte) error
}

// We use a simple broadcast implementation in which items are never invalidated by others.
type simpleBroadcast []byte

func (b simpleBroadcast) Message() []byte                       { return []byte(b) }
func (b simpleBroadcast) Invalidates(memberlist.Broadcast) bool { return false }
func (b simpleBroadcast) Finished()                             {}

func hasNonlocal(clusterPeers []string) bool {
	for _, peer := range clusterPeers {
		if host, _, err := net.SplitHostPort(peer); err == nil {
			peer = host
		}
		if ip := net.ParseIP(peer); ip != nil && !ip.IsLoopback() {
			return true
		} else if ip == nil && strings.ToLower(peer) != "localhost" {
			return true
		}
	}
	return false
}

func isUnroutable(addr string) bool {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	if ip := net.ParseIP(addr); ip != nil && (ip.IsUnspecified() || ip.IsLoopback()) {
		return true // typically 0.0.0.0 or localhost
	} else if ip == nil && strings.ToLower(addr) == "localhost" {
		return true
	}
	return false
}

func isAny(addr string) bool {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	return addr == "" || net.ParseIP(addr).IsUnspecified()
}

func removeOldPeer(old []peer, addr string) []peer {
	new := make([]peer, 0, len(old))
	for _, p := range old {
		if p.Address() != addr {
			new = append(new, p)
		}
	}

	return new
}

type staticAddress string

// Run implements the Discoverer interface.
func (s staticAddress) Run(ctx context.Context, ch chan<- []*targetgroup.Group) {
	select {
	case ch <- []*targetgroup.Group{
		{
			Targets: []model.LabelSet{{
				model.AddressLabel: model.LabelValue(s),
			}},
			Source: string(s),
		},
	}:
	case <-ctx.Done():
	}
}
