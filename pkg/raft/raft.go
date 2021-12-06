package raft

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/facebook/fbthrift/thrift/lib/go/thrift"
	"github.com/golang/glog"
	"github.com/vesoft-inc/nebula-go/v2/nebula"
	"github.com/vesoft-inc/nebula-go/v2/raftex"
)

const defaultRaftPort = 9780

type RaftPeer struct {
	host   string
	port   int
	client *raftex.RaftexServiceClient
	// sshPort int
}

func (r *RaftPeer) GetHost() string {
	return r.host
}

func (r *RaftPeer) GetPort() int {
	return r.port
}

func (r *RaftPeer) Close() {
	if r.client != nil {
		r.client.Close()
	}
}

// TODO make a singleton
type RaftCluster struct {
	hosts           map[string]*RaftPeer
	lock            sync.Mutex
	leader          string
	spaceID         nebula.GraphSpaceID
	partID          nebula.GraphSpaceID
	refreshInterval time.Duration
	lastTick        time.Time
}

func (c *RaftCluster) GetPeers() []*RaftPeer {
	peers := []*RaftPeer{}
	for _, p := range c.hosts {
		peers = append(peers, p)
	}

	return peers
}

func NewRaftCluster(spaceID nebula.GraphSpaceID, partID nebula.GraphSpaceID) *RaftCluster {
	return &RaftCluster{
		hosts:           make(map[string]*RaftPeer),
		spaceID:         spaceID,
		partID:          partID,
		refreshInterval: 1 * time.Millisecond,
	}
}

func (c *RaftCluster) Close() {
	for _, r := range c.hosts {
		r.client.Close()
	}
}

// TODO close me
// func (c *RaftCluster) refreshLeader() {
// 	go func() {
// 		for {
// 			time.Sleep(32 * time.Millisecond)
// 			c.doGetLeader()
// 		}
// 	}()
// }

func (c *RaftCluster) GetLeader() (string, error) {
	if time.Since(c.lastTick) > c.refreshInterval {
		go func() {
			c.lock.Lock()
			defer c.lock.Unlock()

			if time.Since(c.lastTick) < c.refreshInterval {
				return
			}

			c.doGetLeader()
			c.lastTick = time.Now()
		}()
	}

	for {
		// FIXME:dirty hack
		if c.leader != "" {
			break
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}

	return c.leader, nil
}

func (c *RaftCluster) doGetLeader() {
	var leaderTerm int64 = 0

	for id, inst := range c.hosts {
		req := raftex.GetStateRequest{
			Space: int32(c.spaceID),
			Part:  int32(c.partID),
		}
		glog.V(2).Infof("getting raft state")
		resp, err := inst.client.GetState(&req)
		glog.V(2).Infof("done getting raft %s state: %+v", id, resp)
		if err != nil {
			glog.Errorf("error retrieving leader info from %s, err: %+v\n", id, err)
			if strings.Contains(err.Error(), "i/o timeout") ||
				strings.Contains(err.Error(), "Invalid data length") ||
				strings.Contains(err.Error(), "Not enough frame size") ||
				strings.Contains(err.Error(), "out of sequence response") ||
				strings.Contains(err.Error(), "Bad version in") ||
				strings.Contains(err.Error(), "broken pipe") ||
				strings.Contains(err.Error(), "EOF") {
				client, err := newRaftClient(inst.host, inst.port)
				if err == nil {
					inst.client = client
				} else {
					glog.Errorf("failed connecting to raft: %+v", err)
				}
			} else {
				glog.Fatalf("unknown error: %+v", err)
			}

			continue
		} else {
			if resp.ErrorCode != raftex.ErrorCode_SUCCEEDED {
				glog.Fatalf("failed getting raft status: %+v", resp)
			}
		}

		if resp.IsLeader {
			glog.V(2).Infof("found leader of term: %d, leader: %s\n", resp.Term, id)
			if resp.Term > int64(leaderTerm) {
				glog.V(2).Infof("setting leader to: %s\n", id)
				c.leader = id
				leaderTerm = resp.Term
			}
		}
	}
}

func parseHost(h string) (host string, port int, err error) {
	i := strings.Index(h, ":")
	if i == -1 {
		host, port = h, defaultRaftPort
		return
	}

	host = h[:i]
	port, err = strconv.Atoi(h[i+1:])
	if err != nil {
		err = fmt.Errorf("error parsing raft host %s: %+v", h, err)
	}
	return
}

func (c *RaftCluster) RegisterHost(id string, host string) error {
	h, p, err := parseHost(host)
	if err != nil {
		return err
	}

	return c.RegisterHostWithPort(id, h, p)
}

func (c *RaftCluster) RegisterHostWithPort(id string, host string, port int) error {
	glog.Infof("registring raft host: %s, port: %d", host, port)
	client, err := newRaftClient(host, port)
	glog.Infof("done registring raft host: %s, port: %d", host, port)
	if err != nil {
		return err
	}

	peer := &RaftPeer{
		host:   host,
		port:   port,
		client: client,
	}

	c.hosts[id] = peer
	return nil
}

func newRaftClient(host string, port int) (*raftex.RaftexServiceClient, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	timeout := thrift.SocketTimeout(4 * time.Second)
	frameMaxLength := uint32(math.MaxUint32)
	sockAddr := thrift.SocketAddr(addr)
	sock, err := thrift.NewSocket(timeout, sockAddr)
	if err != nil {
		// return nil, fmt.Errorf("failed creating a net.Conn-backed Transport,: %+v", err)
		return nil, fmt.Errorf("failed creating a net.Conn-backed Transport,: %+v", err)
	}

	// Set transport buffer
	bufferedTranFactory := thrift.NewBufferedTransportFactory(65536)
	transport := thrift.NewFramedTransportMaxLength(bufferedTranFactory.GetTransport(sock), frameMaxLength)
	pf := thrift.NewBinaryProtocolFactoryDefault()

	client := raftex.NewRaftexServiceClientFactory(transport, pf)
	if err := client.Open(); err != nil {
		return nil, err
	}

	if !client.IsOpen() {
		return nil, fmt.Errorf("transport is off")
	}

	return client, nil
}
