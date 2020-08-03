package initialsync

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/kevinms/leakybucket-go"
	streamhelpers "github.com/libp2p/go-libp2p-core/helpers"
	"github.com/libp2p/go-libp2p-core/mux"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/pkg/errors"
	eth "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	"github.com/prysmaticlabs/prysm/beacon-chain/blockchain"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	"github.com/prysmaticlabs/prysm/beacon-chain/flags"
	"github.com/prysmaticlabs/prysm/beacon-chain/p2p"
	"github.com/prysmaticlabs/prysm/beacon-chain/p2p/peers"
	prysmsync "github.com/prysmaticlabs/prysm/beacon-chain/sync"
	p2ppb "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	"github.com/prysmaticlabs/prysm/shared/mathutil"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/prysmaticlabs/prysm/shared/rand"
	"github.com/prysmaticlabs/prysm/shared/roughtime"
	"github.com/sirupsen/logrus"
	"go.opencensus.io/trace"
)

const (
	// maxPendingRequests limits how many concurrent fetch request one can initiate.
	maxPendingRequests = 64
	// peersPercentagePerRequest caps percentage of peers to be used in a request.
	peersPercentagePerRequest = 0.75
	// handshakePollingInterval is a polling interval for checking the number of received handshakes.
	handshakePollingInterval = 5 * time.Second
	// peerLocksPollingInterval is a polling interval for checking if there are stale peer locks.
	peerLocksPollingInterval = 5 * time.Minute
	// peerLockMaxAge is maximum time before stale lock is purged.
	peerLockMaxAge = 60 * time.Minute
	// nonSkippedSlotsFullSearchEpochs how many epochs to check in full, before resorting to random
	// sampling of slots once per epoch
	nonSkippedSlotsFullSearchEpochs = 10
	// peerFilterCapacityWeight defines how peer's capacity affects peer's score. Provided as
	// percentage, i.e. 0.3 means capacity will determine 30% of peer's score.
	peerFilterCapacityWeight = 0.2
)

var (
	errNoPeersAvailable = errors.New("no peers available, waiting for reconnect")
	errFetcherCtxIsDone = errors.New("fetcher's context is done, reinitialize")
	errSlotIsTooHigh    = errors.New("slot is higher than the finalized slot")
)

// blocksFetcherConfig is a config to setup the block fetcher.
type blocksFetcherConfig struct {
	headFetcher blockchain.HeadFetcher
	p2p         p2p.P2P
}

// blocksFetcher is a service to fetch chain data from peers.
// On an incoming requests, requested block range is evenly divided
// among available peers (for fair network load distribution).
type blocksFetcher struct {
	sync.Mutex
	ctx             context.Context
	cancel          context.CancelFunc
	rand            *rand.Rand
	headFetcher     blockchain.HeadFetcher
	p2p             p2p.P2P
	blocksPerSecond uint64
	rateLimiter     *leakybucket.Collector
	peerLocks       map[peer.ID]*peerLock
	fetchRequests   chan *fetchRequestParams
	fetchResponses  chan *fetchRequestResponse
	quit            chan struct{} // termination notifier
}

// peerLock restricts fetcher actions on per peer basis. Currently, used for rate limiting.
type peerLock struct {
	sync.Mutex
	accessed time.Time
}

// fetchRequestParams holds parameters necessary to schedule a fetch request.
type fetchRequestParams struct {
	ctx   context.Context // if provided, it is used instead of global fetcher's context
	start uint64          // starting slot
	count uint64          // how many slots to receive (fetcher may return fewer slots)
}

// fetchRequestResponse is a combined type to hold results of both successful executions and errors.
// Valid usage pattern will be to check whether result's `err` is nil, before using `blocks`.
type fetchRequestResponse struct {
	start, count uint64
	blocks       []*eth.SignedBeaconBlock
	err          error
}

// newBlocksFetcher creates ready to use fetcher.
func newBlocksFetcher(ctx context.Context, cfg *blocksFetcherConfig) *blocksFetcher {
	blocksPerSecond := flags.Get().BlockBatchLimit
	allowedBlocksBurst := flags.Get().BlockBatchLimitBurstFactor * flags.Get().BlockBatchLimit
	// Allow fetcher to go almost to the full burst capacity (less a single batch).
	rateLimiter := leakybucket.NewCollector(
		float64(blocksPerSecond), int64(allowedBlocksBurst-blocksPerSecond),
		false /* deleteEmptyBuckets */)

	ctx, cancel := context.WithCancel(ctx)
	return &blocksFetcher{
		ctx:             ctx,
		cancel:          cancel,
		rand:            rand.NewGenerator(),
		headFetcher:     cfg.headFetcher,
		p2p:             cfg.p2p,
		blocksPerSecond: uint64(blocksPerSecond),
		rateLimiter:     rateLimiter,
		peerLocks:       make(map[peer.ID]*peerLock),
		fetchRequests:   make(chan *fetchRequestParams, maxPendingRequests),
		fetchResponses:  make(chan *fetchRequestResponse, maxPendingRequests),
		quit:            make(chan struct{}),
	}
}

// start boots up the fetcher, which starts listening for incoming fetch requests.
func (f *blocksFetcher) start() error {
	select {
	case <-f.ctx.Done():
		return errFetcherCtxIsDone
	default:
		go f.loop()
		return nil
	}
}

// stop terminates all fetcher operations.
func (f *blocksFetcher) stop() {
	defer func() {
		if f.rateLimiter != nil {
			f.rateLimiter.Free()
			f.rateLimiter = nil
		}
	}()
	f.cancel()
	<-f.quit // make sure that loop() is done
}

// requestResponses exposes a channel into which fetcher pushes generated request responses.
func (f *blocksFetcher) requestResponses() <-chan *fetchRequestResponse {
	return f.fetchResponses
}

// loop is a main fetcher loop, listens for incoming requests/cancellations, forwards outgoing responses.
func (f *blocksFetcher) loop() {
	defer close(f.quit)

	// Wait for all loop's goroutines to finish, and safely release resources.
	wg := &sync.WaitGroup{}
	defer func() {
		wg.Wait()
		close(f.fetchResponses)
	}()

	// Periodically remove stale peer locks.
	go func() {
		ticker := time.NewTicker(peerLocksPollingInterval)
		for {
			select {
			case <-ticker.C:
				f.removeStalePeerLocks(peerLockMaxAge)
			case <-f.ctx.Done():
				ticker.Stop()
				return
			}
		}
	}()

	// Main loop.
	for {
		// Make sure there is are available peers before processing requests.
		if _, err := f.waitForMinimumPeers(f.ctx); err != nil {
			log.Error(err)
		}

		select {
		case <-f.ctx.Done():
			log.Debug("Context closed, exiting goroutine (blocks fetcher)")
			return
		case req := <-f.fetchRequests:
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case <-f.ctx.Done():
				case f.fetchResponses <- f.handleRequest(req.ctx, req.start, req.count):
				}
			}()
		}
	}
}

// scheduleRequest adds request to incoming queue.
func (f *blocksFetcher) scheduleRequest(ctx context.Context, start, count uint64) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	request := &fetchRequestParams{
		ctx:   ctx,
		start: start,
		count: count,
	}
	select {
	case <-f.ctx.Done():
		return errFetcherCtxIsDone
	case f.fetchRequests <- request:
	}
	return nil
}

// handleRequest parses fetch request and forwards it to response builder.
func (f *blocksFetcher) handleRequest(ctx context.Context, start, count uint64) *fetchRequestResponse {
	ctx, span := trace.StartSpan(ctx, "initialsync.handleRequest")
	defer span.End()

	response := &fetchRequestResponse{
		start:  start,
		count:  count,
		blocks: []*eth.SignedBeaconBlock{},
		err:    nil,
	}

	if ctx.Err() != nil {
		response.err = ctx.Err()
		return response
	}

	headEpoch := helpers.SlotToEpoch(f.headFetcher.HeadSlot())
	finalizedEpoch, peerIDs := f.p2p.Peers().BestFinalized(params.BeaconConfig().MaxPeersToSync, headEpoch)
	if len(peerIDs) == 0 {
		response.err = errNoPeersAvailable
		return response
	}

	// Short circuit start far exceeding the highest finalized epoch in some infinite loop.
	highestFinalizedSlot := helpers.StartSlot(finalizedEpoch + 1)
	if start > highestFinalizedSlot {
		response.err = fmt.Errorf("%v, slot: %d, higest finilized slot: %d",
			errSlotIsTooHigh, start, highestFinalizedSlot)
		return response
	}

	response.blocks, response.err = f.fetchBlocksFromPeer(ctx, start, count, peerIDs)
	return response
}

// fetchBlocksFromPeer fetches blocks from a single randomly selected peer.
func (f *blocksFetcher) fetchBlocksFromPeer(
	ctx context.Context,
	start, count uint64,
	peerIDs []peer.ID,
) ([]*eth.SignedBeaconBlock, error) {
	ctx, span := trace.StartSpan(ctx, "initialsync.fetchBlocksFromPeer")
	defer span.End()

	var blocks []*eth.SignedBeaconBlock
	var err error
	peerIDs, err = f.filterPeers(ctx, peerIDs, peersPercentagePerRequest)
	if err != nil {
		return blocks, err
	}
	if len(peerIDs) == 0 {
		return blocks, errNoPeersAvailable
	}
	req := &p2ppb.BeaconBlocksByRangeRequest{
		StartSlot: start,
		Count:     count,
		Step:      1,
	}
	for i := 0; i < len(peerIDs); i++ {
		if blocks, err = f.requestBlocks(ctx, req, peerIDs[i]); err == nil {
			return blocks, err
		}
	}
	return blocks, nil
}

// requestBlocks is a wrapper for handling BeaconBlocksByRangeRequest requests/streams.
func (f *blocksFetcher) requestBlocks(
	ctx context.Context,
	req *p2ppb.BeaconBlocksByRangeRequest,
	peerID peer.ID,
) ([]*eth.SignedBeaconBlock, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	l := f.getPeerLock(peerID)
	if l == nil {
		return nil, errors.New("cannot obtain lock")
	}
	l.Lock()
	log.WithFields(logrus.Fields{
		"peer":     peerID,
		"start":    req.StartSlot,
		"count":    req.Count,
		"step":     req.Step,
		"capacity": f.rateLimiter.Remaining(peerID.String()),
		"score":    f.p2p.Peers().Scorers().BlockProviderScorer().FormatScorePretty(peerID),
	}).Debug("Requesting blocks")
	if f.rateLimiter.Remaining(peerID.String()) < int64(req.Count) {
		log.WithField("peer", peerID).Debug("Slowing down for rate limit")
		timer := time.NewTimer(f.rateLimiter.TillEmpty(peerID.String()))
		select {
		case <-f.ctx.Done():
			timer.Stop()
			return nil, errFetcherCtxIsDone
		case <-timer.C:
			// Peer has gathered enough capacity to be polled again.
		}
	}
	f.rateLimiter.Add(peerID.String(), int64(req.Count))
	l.Unlock()
	stream, err := f.p2p.Send(ctx, req, p2p.RPCBlocksByRangeTopic, peerID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := streamhelpers.FullClose(stream); err != nil && err.Error() != mux.ErrReset.Error() {
			log.WithError(err).Errorf("Failed to close stream with protocol %s", stream.Protocol())
		}
	}()

	resp := make([]*eth.SignedBeaconBlock, 0, req.Count)
	for i := uint64(0); ; i++ {
		isFirstChunk := i == 0
		blk, err := prysmsync.ReadChunkedBlock(stream, f.p2p, isFirstChunk)
		if err == io.EOF {
			break
		}
		// exit if more than max request blocks are returned
		if i >= params.BeaconNetworkConfig().MaxRequestBlocks {
			break
		}
		if err != nil {
			return nil, err
		}
		resp = append(resp, blk)
	}

	return resp, nil
}

// getPeerLock returns peer lock for a given peer. If lock is not found, it is created.
func (f *blocksFetcher) getPeerLock(peerID peer.ID) *peerLock {
	f.Lock()
	defer f.Unlock()
	if lock, ok := f.peerLocks[peerID]; ok {
		lock.accessed = roughtime.Now()
		return lock
	}
	f.peerLocks[peerID] = &peerLock{
		Mutex:    sync.Mutex{},
		accessed: roughtime.Now(),
	}
	return f.peerLocks[peerID]
}

// removeStalePeerLocks is a cleanup procedure which removes stale locks.
func (f *blocksFetcher) removeStalePeerLocks(age time.Duration) {
	f.Lock()
	defer f.Unlock()
	for peerID, lock := range f.peerLocks {
		if time.Since(lock.accessed) >= age {
			lock.Lock()
			delete(f.peerLocks, peerID)
			lock.Unlock()
		}
	}
}

// selectFailOverPeer randomly selects fail over peer from the list of available peers.
func (f *blocksFetcher) selectFailOverPeer(excludedPID peer.ID, peerIDs []peer.ID) (peer.ID, error) {
	if len(peerIDs) == 0 {
		return "", errNoPeersAvailable
	}
	if len(peerIDs) == 1 && peerIDs[0] == excludedPID {
		return "", errNoPeersAvailable
	}

	ind := f.rand.Int() % len(peerIDs)
	if peerIDs[ind] == excludedPID {
		return f.selectFailOverPeer(excludedPID, append(peerIDs[:ind], peerIDs[ind+1:]...))
	}
	return peerIDs[ind], nil
}

// waitForMinimumPeers spins and waits up until enough peers are available.
func (f *blocksFetcher) waitForMinimumPeers(ctx context.Context) ([]peer.ID, error) {
	required := params.BeaconConfig().MaxPeersToSync
	if flags.Get().MinimumSyncPeers < required {
		required = flags.Get().MinimumSyncPeers
	}
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		headEpoch := helpers.SlotToEpoch(f.headFetcher.HeadSlot())
		_, peerIDs := f.p2p.Peers().BestFinalized(params.BeaconConfig().MaxPeersToSync, headEpoch)
		if len(peerIDs) >= required {
			return peerIDs, nil
		}
		log.WithFields(logrus.Fields{
			"suitable": len(peerIDs),
			"required": required,
		}).Info("Waiting for enough suitable peers before syncing")
		time.Sleep(handshakePollingInterval)
	}
}

// filterPeers returns transformed list of peers,
// weight ordered or randomized, constrained if necessary.
func (f *blocksFetcher) filterPeers(ctx context.Context, peerIDs []peer.ID, ratio float64) ([]peer.ID, error) {
	ctx, span := trace.StartSpan(ctx, "initialsync.filterPeers")
	defer span.End()

	if len(peerIDs) == 0 {
		return peerIDs, nil
	}
	scorer := f.p2p.Peers().Scorers().BlockProviderScorer()

	// Sort peers by their score (in descending order), non-responsive peers will be constantly
	// pushed down the list and trimmed when percentage is selected.
	peerIDs = scorer.Sorted(peerIDs)

	// Select sub-sample from peers (honoring min-max invariants).
	limit := uint64(math.Round(float64(len(peerIDs)) * ratio))
	limit = mathutil.Max(limit, uint64(flags.Get().MinimumSyncPeers))
	limit = mathutil.Min(limit, uint64(len(peerIDs)))
	peerIDs = peerIDs[:limit]

	// Order peers by score and remaining capacity, effectively turning in-order
	// round robin peer processing into a weighted one (peers with higher scores and higher
	// remaining capacity are preferred). The effect of capacity on overall score is controlled
	// via peerFilterCapacityWeight param.
	sort.SliceStable(peerIDs, func(i, j int) bool {
		aggScore := func(peerID peer.ID) float64 {
			blockProviderScore := scorer.Score(peerID)
			l := f.getPeerLock(peerID)
			if l == nil {
				return blockProviderScore
			}
			l.Lock()
			defer l.Unlock()
			remaining, capacity := float64(f.rateLimiter.Remaining(peerID.String())), float64(f.rateLimiter.Capacity())
			if remaining < float64(f.blocksPerSecond) {
				// When no capacity for a good peer left, allow less performant peer to take a chance.
				return 0.0
			}
			capScore := remaining / capacity
			overallScore := blockProviderScore*(1.0-peerFilterCapacityWeight) + capScore*peerFilterCapacityWeight
			overallScore = math.Round(overallScore*peers.ScoreRoundingFactor) / peers.ScoreRoundingFactor
			return overallScore
		}
		return aggScore(peerIDs[i]) > aggScore(peerIDs[j])
	})

	return peerIDs, nil
}

// nonSkippedSlotAfter checks slots after the given one in an attempt to find a non-empty future slot.
// For efficiency only one random slot is checked per epoch, so returned slot might not be the first
// non-skipped slot. This shouldn't be a problem, as in case of adversary peer, we might get incorrect
// data anyway, so code that relies on this function must be robust enough to re-request, if no progress
// is possible with a returned value.
func (f *blocksFetcher) nonSkippedSlotAfter(ctx context.Context, slot uint64) (uint64, error) {
	ctx, span := trace.StartSpan(ctx, "initialsync.nonSkippedSlotAfter")
	defer span.End()

	headEpoch := helpers.SlotToEpoch(f.headFetcher.HeadSlot())
	finalizedEpoch, peerIDs := f.p2p.Peers().BestFinalized(params.BeaconConfig().MaxPeersToSync, headEpoch)
	log.WithFields(logrus.Fields{
		"start":          slot,
		"headEpoch":      headEpoch,
		"finalizedEpoch": finalizedEpoch,
	}).Debug("Searching for non-skipped slot")
	// Exit early, if no peers with high enough finalized epoch are found.
	if finalizedEpoch <= headEpoch {
		return 0, errSlotIsTooHigh
	}
	var err error
	peerIDs, err = f.filterPeers(ctx, peerIDs, peersPercentagePerRequest)
	if err != nil {
		return 0, err
	}
	if len(peerIDs) == 0 {
		return 0, errNoPeersAvailable
	}

	slotsPerEpoch := params.BeaconConfig().SlotsPerEpoch
	peerInd := 0

	fetch := func(pid peer.ID, start, count, step uint64) (uint64, error) {
		req := &p2ppb.BeaconBlocksByRangeRequest{
			StartSlot: start,
			Count:     count,
			Step:      step,
		}
		blocks, err := f.requestBlocks(ctx, req, pid)
		if err != nil {
			return 0, err
		}
		if len(blocks) > 0 {
			for _, block := range blocks {
				if block.Block.Slot > slot {
					return block.Block.Slot, nil
				}
			}
		}
		return 0, nil
	}

	// Start by checking several epochs fully, w/o resorting to random sampling.
	start := slot + 1
	end := start + nonSkippedSlotsFullSearchEpochs*slotsPerEpoch
	for ind := start; ind < end; ind += slotsPerEpoch {
		nextSlot, err := fetch(peerIDs[peerInd%len(peerIDs)], ind, slotsPerEpoch, 1)
		if err != nil {
			return 0, err
		}
		if nextSlot > slot {
			return nextSlot, nil
		}
		peerInd++
	}

	// Quickly find the close enough epoch where a non-empty slot definitely exists.
	// Only single random slot per epoch is checked - allowing to move forward relatively quickly.
	slot = slot + nonSkippedSlotsFullSearchEpochs*slotsPerEpoch
	upperBoundSlot := helpers.StartSlot(finalizedEpoch + 1)
	for ind := slot + 1; ind < upperBoundSlot; ind += (slotsPerEpoch * slotsPerEpoch) / 2 {
		start := ind + uint64(f.rand.Intn(int(slotsPerEpoch)))
		nextSlot, err := fetch(peerIDs[peerInd%len(peerIDs)], start, slotsPerEpoch/2, slotsPerEpoch)
		if err != nil {
			return 0, err
		}
		peerInd++
		if nextSlot > slot && upperBoundSlot >= nextSlot {
			upperBoundSlot = nextSlot
			break
		}
	}

	// Epoch with non-empty slot is located. Check all slots within two nearby epochs.
	if upperBoundSlot > slotsPerEpoch {
		upperBoundSlot -= slotsPerEpoch
	}
	upperBoundSlot = helpers.StartSlot(helpers.SlotToEpoch(upperBoundSlot))
	nextSlot, err := fetch(peerIDs[peerInd%len(peerIDs)], upperBoundSlot, slotsPerEpoch*2, 1)
	if err != nil {
		return 0, err
	}
	if nextSlot < slot || helpers.StartSlot(finalizedEpoch+1) < nextSlot {
		return 0, errors.New("invalid range for non-skipped slot")
	}
	return nextSlot, nil
}

// bestFinalizedSlot returns the highest finalized slot of the majority of connected peers.
func (f *blocksFetcher) bestFinalizedSlot() uint64 {
	headEpoch := helpers.SlotToEpoch(f.headFetcher.HeadSlot())
	finalizedEpoch, _ := f.p2p.Peers().BestFinalized(params.BeaconConfig().MaxPeersToSync, headEpoch)
	return helpers.StartSlot(finalizedEpoch)
}
