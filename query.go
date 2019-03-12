package dht

import (
	"context"
	"sync"

	kpeerset "github.com/libp2p/go-libp2p-kad-dht/kpeerset"

	u "github.com/ipfs/go-ipfs-util"
	logging "github.com/ipfs/go-log"
	todoctr "github.com/ipfs/go-todocounter"
	process "github.com/jbenet/goprocess"
	ctxproc "github.com/jbenet/goprocess/context"
	kb "github.com/libp2p/go-libp2p-kbucket"
	inet "github.com/libp2p/go-libp2p-net"
	peer "github.com/libp2p/go-libp2p-peer"
	pset "github.com/libp2p/go-libp2p-peer/peerset"
	pstore "github.com/libp2p/go-libp2p-peerstore"
	queue "github.com/libp2p/go-libp2p-peerstore/queue"
	notif "github.com/libp2p/go-libp2p-routing/notifications"
)

var maxQueryConcurrency = AlphaValue

// We "finish" queries with KValue + finishSlop peers but only wait for KValue
// peers. This helps us account for unreachable peers.
var finishSlop = 4

type dhtQuery struct {
	dht         *IpfsDHT
	key         string      // the key we're querying for
	rfunc       recurseFunc // the function to execute per peer
	concurrency int         // the concurrency parameter
}

type dhtQueryResult struct {
	closerPeers []*pstore.PeerInfo // *
}

type dhtQueryRecurseResult struct {
	query                 *dhtQuery
	seen, queried, failed []peer.ID
}

// constructs query
func (dht *IpfsDHT) newQuery(k string, f recurseFunc) *dhtQuery {
	return &dhtQuery{
		key:         k,
		dht:         dht,
		rfunc:       f,
		concurrency: maxQueryConcurrency,
	}
}

// QueryFunc is a function that runs a particular query with a given peer.
// It returns either:
// - the value
// - a list of peers potentially better able to serve the query
// - an error
type recurseFunc func(context.Context, peer.ID) (*dhtQueryResult, error)

func (q *dhtQuery) Run(ctx context.Context, peers []peer.ID) ([]peer.ID, error) {
	res, err := q.Recurse(ctx, peers)
	if err != nil || res == nil {
		return nil, err
	}
	return res.Finish(ctx)
}

// Recurse runs the recurse step of the query. Pass in a list of peers to use first.
func (q *dhtQuery) Recurse(ctx context.Context, peers []peer.ID) (*dhtQueryRecurseResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	runner := newQueryRunner(q)
	return runner.Recurse(ctx, peers)
}

type dhtQueryRunner struct {
	query          *dhtQuery          // query to run
	peersSeen      *pset.PeerSet      // all peers seen. prevent querying same peer 2x
	peersQueried   *pset.PeerSet      // all peers successfully queried.
	peersFailed    *pset.PeerSet      // all peers not successfully queried.
	aPeers         *kpeerset.KPeerSet // k best peers queried.
	peersDialed    *dialQueue         // peers we have dialed to
	peersToQuery   *queue.ChanQueue   // peers remaining to be queried
	peersRemaining todoctr.Counter    // peersToQuery + currently processing

	errs u.MultiErr // result errors. maybe should be a map[peer.ID]error

	rateLimit chan struct{} // processing semaphore
	log       logging.EventLogger

	runCtx context.Context

	proc process.Process
	sync.RWMutex
}

func newQueryRunner(q *dhtQuery) *dhtQueryRunner {
	proc := process.WithParent(process.Background())
	ctx := ctxproc.OnClosingContext(proc)
	peersToQuery := queue.NewChanQueue(ctx, queue.NewXORDistancePQ(string(q.key)))
	r := &dhtQueryRunner{
		query:          q,
		peersRemaining: todoctr.NewSyncCounter(),
		peersSeen:      pset.New(),
		peersQueried:   pset.New(),
		peersFailed:    pset.New(),
		aPeers:         kpeerset.New(AlphaValue, q.key),
		rateLimit:      make(chan struct{}, q.concurrency),
		peersToQuery:   peersToQuery,
		proc:           proc,
	}
	dq, err := newDialQueue(&dqParams{
		ctx:    ctx,
		target: q.key,
		in:     peersToQuery,
		dialFn: r.dialPeer,
		config: dqDefaultConfig(),
	})
	if err != nil {
		panic(err)
	}
	r.peersDialed = dq
	return r
}

func (r *dhtQueryRunner) Recurse(ctx context.Context, peers []peer.ID) (*dhtQueryRecurseResult, error) {
	r.log = logger
	r.runCtx = ctx

	if len(peers) == 0 {
		logger.Warning("Running query with no peers!")
		return nil, nil
	}

	// setup concurrency rate limiting
	for i := 0; i < r.query.concurrency; i++ {
		r.rateLimit <- struct{}{}
	}

	// add all the peers we got first.
	for _, p := range peers {
		r.addPeerToQuery(p)
	}

	// go do this thing.
	// do it as a child proc to make sure Run exits
	// ONLY AFTER spawn workers has exited.
	r.proc.Go(r.spawnWorkers)

	// so workers are working.

	// now, if the context finishes, close the proc.
	// we have to do it here because the logic before is setup, which
	// should run without closing the proc.
	ctxproc.CloseAfterContext(r.proc, ctx)

	var err error
	select {
	case <-r.peersRemaining.Done():
		// Cleanup workers.
		r.proc.Close()

		r.RLock()

		// if every query to every peer failed, something must be very wrong.
		if len(r.errs) > 0 && len(r.errs) == r.peersSeen.Size() {
			logger.Debugf("query errs: %s", r.errs)
			err = r.errs[0]
		}

		r.RUnlock()
	case <-r.proc.Closed():
		r.RLock()
		defer r.RUnlock()
		err = r.runCtx.Err()
	}
	return &dhtQueryRecurseResult{
		query:   r.query,
		seen:    r.peersSeen.Peers(),
		queried: r.peersQueried.Peers(),
		failed:  r.peersFailed.Peers(),
	}, err
}

func (r *dhtQueryRecurseResult) Finish(ctx context.Context) ([]peer.ID, error) {
	return r.FinishWith(ctx, nil)
}

func (r *dhtQueryRecurseResult) FinishWith(ctx context.Context, fn func(context.Context, peer.ID) error) ([]peer.ID, error) {
	// Get a sorted list of peers to query.
	failed := make(map[peer.ID]bool, len(r.failed))
	for _, p := range r.failed {
		failed[p] = true
	}

	succeeded := make(map[peer.ID]bool, len(r.queried))
	for _, p := range r.queried {
		if !failed[p] {
			succeeded[p] = true
		}
	}

	closest := make([]peer.ID, 0, len(r.seen))
	for _, p := range r.seen {
		if failed[p] {
			continue
		}
		closest = append(closest, p)
	}
	closest = kb.SortClosestPeers(closest, kb.ConvertKey(r.query.key))

	// Query them.
	sloppyK := KValue + finishSlop
	bucket := make([]peer.ID, 0, KValue)
	workQ := make(chan peer.ID)
	resultQ := make(chan peer.ID, sloppyK)

	newQuery := fn != nil
	if !newQuery {
		fn = func(ctx context.Context, p peer.ID) error {
			_, err := r.query.rfunc(ctx, p)
			return err
		}
	}

	var wg sync.WaitGroup
	defer wg.Wait()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	wg.Add(sloppyK)
	for i := 0; i < sloppyK; i++ {
		go func() {
			defer wg.Done()
			for p := range workQ {
				if fn(ctx, p) == nil {
					resultQ <- p
					return
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(resultQ)
	}()

	// No need to handle the context, assuming the _user_ does in rfunc.

	for len(bucket) < KValue && len(closest) > 0 {
		if !newQuery && succeeded[closest[0]] {
			// no need to re-query this peer as we've already sent
			// them this query.
			bucket = append(bucket, closest[0])
			closest = closest[1:]
			continue
		}

		select {
		case workQ <- closest[0]:
			closest = closest[1:]
		case successPeer, ok := <-resultQ:
			if !ok {
				return bucket, ctx.Err()
			}
			bucket = append(bucket, successPeer)
		}
	}

	close(workQ)

	for len(bucket) < KValue {
		p, ok := <-resultQ
		if !ok {
			break
		}
		bucket = append(bucket, p)
	}

	return bucket, ctx.Err()
}

func (r *dhtQueryRunner) addPeerToQuery(next peer.ID) {
	// if new peer is ourselves...
	if next == r.query.dht.self {
		r.log.Debug("addPeerToQuery skip self")
		return
	}

	if !r.peersSeen.TryAdd(next) {
		return
	}

	notif.PublishQueryEvent(r.runCtx, &notif.QueryEvent{
		Type: notif.AddingPeer,
		ID:   next,
	})

	if !r.aPeers.Check(next) {
		return
	}

	r.peersRemaining.Increment(1)
	select {
	case r.peersToQuery.EnqChan <- next:
	case <-r.proc.Closing():
	}
}

func (r *dhtQueryRunner) spawnWorkers(proc process.Process) {
	for {
		select {
		case <-r.peersRemaining.Done():
			return

		case <-r.proc.Closing():
			return

		case <-r.rateLimit:
			ch := r.peersDialed.Consume()
			select {
			case p, ok := <-ch:
				if !ok {
					// this signals context cancellation.
					return
				}
				// do it as a child func to make sure Run exits
				// ONLY AFTER spawn workers has exited.
				proc.Go(func(proc process.Process) {
					r.queryPeer(proc, p)
				})
			case <-r.proc.Closing():
				return
			case <-r.peersRemaining.Done():
				return
			}
		}
	}
}

func (r *dhtQueryRunner) dialPeer(ctx context.Context, p peer.ID) error {
	if !r.aPeers.Check(p) {
		// Don't bother with this peer. We'll skip it in the query phase as well.
		return nil
	}

	// short-circuit if we're already connected.
	if r.query.dht.host.Network().Connectedness(p) == inet.Connected {
		return nil
	}

	logger.Debug("not connected. dialing.")
	notif.PublishQueryEvent(r.runCtx, &notif.QueryEvent{
		Type: notif.DialingPeer,
		ID:   p,
	})

	pi := pstore.PeerInfo{ID: p}
	if err := r.query.dht.host.Connect(ctx, pi); err != nil {
		logger.Debugf("error connecting: %s", err)
		notif.PublishQueryEvent(r.runCtx, &notif.QueryEvent{
			Type:  notif.QueryError,
			Extra: err.Error(),
			ID:    p,
		})

		r.Lock()
		r.errs = append(r.errs, err)
		r.Unlock()

		r.peersFailed.Add(p)

		// This peer is dropping out of the race.
		r.peersRemaining.Decrement(1)
		return err
	}
	logger.Debugf("connected. dial success.")
	return nil
}

func (r *dhtQueryRunner) queryPeer(proc process.Process, p peer.ID) {
	// ok let's do this!

	// create a context from our proc.
	ctx := ctxproc.OnClosingContext(proc)

	// make sure we do this when we exit
	defer func() {
		// signal we're done processing peer p
		r.peersRemaining.Decrement(1)
		r.rateLimit <- struct{}{}
	}()

	if !r.aPeers.Check(p) {
		// Don't bother with this peer.
		return
	}

	r.peersQueried.Add(p)

	// finally, run the query against this peer
	res, err := r.query.rfunc(ctx, p)

	if err == nil {
		r.aPeers.Add(p)
	} else {
		r.peersFailed.Add(p)
	}

	if err != nil {
		logger.Debugf("ERROR worker for: %v %v", p, err)
		r.Lock()
		r.errs = append(r.errs, err)
		r.Unlock()
	} else if len(res.closerPeers) > 0 {
		logger.Debugf("PEERS CLOSER -- worker for: %v (%d closer peers)", p, len(res.closerPeers))
		for _, next := range res.closerPeers {
			if next.ID == r.query.dht.self { // don't add self.
				logger.Debugf("PEERS CLOSER -- worker for: %v found self", p)
				continue
			}

			// add their addresses to the dialer's peerstore
			r.query.dht.peerstore.AddAddrs(next.ID, next.Addrs, pstore.TempAddrTTL)
			r.addPeerToQuery(next.ID)
			logger.Debugf("PEERS CLOSER -- worker for: %v added %v (%v)", p, next.ID, next.Addrs)
		}
	} else {
		logger.Debugf("QUERY worker for: %v - not found, and no closer peers.", p)
	}
}
