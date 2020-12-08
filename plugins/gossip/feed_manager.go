// SPDX-License-Identifier: MIT

package gossip

import (
	"context"
	"math"
	"sync"

	"github.com/cryptix/go/logging"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/go-kit/kit/metrics"
	"github.com/pkg/errors"
	"go.cryptoscope.co/librarian"
	"go.cryptoscope.co/luigi"
	"go.cryptoscope.co/margaret"
	"go.cryptoscope.co/margaret/multilog"
	"go.cryptoscope.co/muxrpc"

	"go.cryptoscope.co/ssb"
	"go.cryptoscope.co/ssb/internal/luigiutils"
	"go.cryptoscope.co/ssb/internal/mutil"
	"go.cryptoscope.co/ssb/internal/storedrefs"
	"go.cryptoscope.co/ssb/internal/transform"
	"go.cryptoscope.co/ssb/message"
	refs "go.mindeco.de/ssb-refs"
)

// FeedManager handles serving gossip about User Feeds.
type FeedManager struct {
	rootCtx context.Context

	ReceiveLog margaret.Log
	UserFeeds  multilog.MultiLog
	logger     logging.Interface

	liveFeeds    map[string]*luigiutils.MultiSink
	liveFeedsMut sync.Mutex

	// metrics
	sysGauge metrics.Gauge
	sysCtr   metrics.Counter
}

// NewFeedManager returns a new FeedManager used for gossiping about User
// Feeds.
func NewFeedManager(
	ctx context.Context,
	rxlog margaret.Log,
	userFeeds multilog.MultiLog,
	info logging.Interface,
	sysGauge metrics.Gauge,
	sysCtr metrics.Counter,
) *FeedManager {
	fm := &FeedManager{
		ReceiveLog: rxlog,
		UserFeeds:  userFeeds,
		logger:     info,
		rootCtx:    ctx,
		sysCtr:     sysCtr,
		sysGauge:   sysGauge,
		liveFeeds:  make(map[string]*luigiutils.MultiSink),
	}
	// QUESTION: How should the error case be handled?
	go fm.serveLiveFeeds()
	return fm
}

func (m *FeedManager) pour(ctx context.Context, val interface{}, err error) error {
	m.liveFeedsMut.Lock()
	defer m.liveFeedsMut.Unlock()

	logger := log.With(m.logger, "event", "live-pour")

	if err != nil {
		if luigi.IsEOS(err) {
			return nil
		}
		level.Error(logger).Log("msg", "pour failed", "err", err)
		return err
	}

	msg := val.(refs.Message)
	author := msg.Author()
	sink, ok := m.liveFeeds[author.Ref()]
	if !ok {
		return nil
	}
	err = sink.Pour(ctx, msg.ValueContentJSON())
	return err
}

func (m *FeedManager) serveLiveFeeds() {
	seqv, err := m.ReceiveLog.Seq().Value()
	if err != nil {
		err = errors.Wrap(err, "failed to get root log sequence")
		panic(err)
	}

	src, err := m.ReceiveLog.Query(
		margaret.Gt(seqv.(margaret.BaseSeq)),
		margaret.Live(true),
	)
	if err != nil {
		panic(err)
	}

	err = luigi.Pump(m.rootCtx, luigi.FuncSink(m.pour), src)
	if err != nil && err != ssb.ErrShuttingDown && err != context.Canceled {
		err = errors.Wrap(err, "error while serving live feed")
		panic(err)
	}
	level.Warn(m.logger).Log("event", "live qry on rxlog exited")
}

func (m *FeedManager) addLiveFeed(
	ctx context.Context,
	sink luigi.Sink,
	ssbID string,
	seq, limit int64,
) error {
	// TODO: ensure all messages make it to the live query
	//  Messages could be lost when written after the non-live portion and
	//  registering to live feed.
	m.liveFeedsMut.Lock()
	defer m.liveFeedsMut.Unlock()

	liveFeed, ok := m.liveFeeds[ssbID]
	if !ok {
		m.liveFeeds[ssbID] = luigiutils.NewMultiSink(seq)
		liveFeed = m.liveFeeds[ssbID]
	}

	if m.sysGauge != nil {
		m.sysGauge.With("part", "gossip-livefeeds").Set(float64(len(m.liveFeeds)))
	}

	until := seq + limit
	if limit == -1 {
		until = math.MaxInt64
	}
	err := liveFeed.Register(ctx, sink, until)
	if err != nil {
		return errors.Wrapf(err, "could not create live stream for client %s", ssbID)
	}
	m.liveFeeds[ssbID] = liveFeed
	// TODO: Remove multiSink from map when complete
	return nil
}

// nonliveLimit returns the upper limit for a CreateStreamHistory request given
// the current User Feeds latest sequence.
func nonliveLimit(
	arg *message.CreateHistArgs,
	curSeq int64,
) int64 {
	if arg.Limit == -1 {
		return -1
	}
	lastSeq := arg.Seq + arg.Limit - 1
	if lastSeq > curSeq {
		lastSeq = curSeq
	}
	return lastSeq - arg.Seq + 1
}

// liveLimit returns the limit for serving the 'live' portion for a
// CreateStreamHistory request given the current User Feeds latest sequence.
func liveLimit(
	arg *message.CreateHistArgs,
	curSeq int64,
) int64 {
	if arg.Limit == -1 {
		return -1
	}

	startSeq := curSeq + 1
	lastSeq := arg.Seq + arg.Limit - 1
	if lastSeq < curSeq {
		return 0
	}
	return lastSeq - startSeq + 1
}

// getLatestSeq returns the latest Sequence number for the given log.
// TODO: this should probably be on margret itself... (ie. observable less way to get the current sequence)
func getLatestSeq(log margaret.Log) (int64, error) {
	latestSeqValue, err := log.Seq().Value()
	if err != nil {
		return 0, errors.Wrapf(err, "failed to observe latest")
	}
	switch v := latestSeqValue.(type) {
	case librarian.UnsetValue: // don't have the feed - nothing to do?
		return 0, nil
	case margaret.BaseSeq:
		return v.Seq(), nil
	default:
		return 0, errors.Errorf("wrong type in index. expected margaret.BaseSeq - got %T", v)
	}
}

// CreateStreamHistory serves the sink a CreateStreamHistory request to the sink.
func (m *FeedManager) CreateStreamHistory(
	ctx context.Context,
	sink luigi.Sink,
	arg *message.CreateHistArgs,
) error {
	if arg.ID == nil {
		return errors.Errorf("bad request: missing id argument")
	}
	feedLogger := log.With(m.logger, "fr", arg.ID.ShortRef())

	// check what we got
	userLog, err := m.UserFeeds.Get(storedrefs.Feed(arg.ID))
	if err != nil {
		return errors.Wrapf(err, "failed to open sublog for user")
	}
	latest, err := getLatestSeq(userLog)
	if err != nil {
		return errors.Wrap(err, "userLog sequence")
	}

	if arg.Seq != 0 {
		arg.Seq--             // our idx is 0 ed
		if arg.Seq > latest { // more than we got
			if arg.Live {
				return m.addLiveFeed(
					ctx, sink,
					arg.ID.Ref(),
					latest,
					liveLimit(arg, latest),
				)
			}
			return errors.Wrap(sink.Close(), "pour: failed to close")
		}
	}
	if arg.Live && arg.Limit == 0 {
		arg.Limit = -1
	}

	// Make query
	limit := nonliveLimit(arg, latest)
	qryArgs := []margaret.QuerySpec{
		margaret.Limit(int(limit)),
		margaret.Reverse(arg.Reverse),
	}

	if arg.Seq > 0 {
		qryArgs = append(qryArgs, margaret.Gte(margaret.BaseSeq(arg.Seq)))
	}

	if arg.Lt > 0 {
		qryArgs = append(qryArgs, margaret.Lt(margaret.BaseSeq(arg.Lt)))
	}

	if arg.Gt > 0 {
		qryArgs = append(qryArgs, margaret.Gt(margaret.BaseSeq(arg.Gt)))
	}

	resolved := mutil.Indirect(m.ReceiveLog, userLog)
	src, err := resolved.Query(qryArgs...)
	if err != nil {
		return errors.Wrapf(err, "invalid user log query")
	}

	switch arg.ID.Algo {
	case refs.RefAlgoFeedSSB1:
		sink = transform.NewKeyValueWrapper(sink, arg.Keys)

	case refs.RefAlgoFeedGabby:
		switch {
		case arg.AsJSON:
			sink = transform.NewKeyValueWrapper(sink, arg.Keys)
		default:
			sink = luigiutils.NewGabbyStreamSink(sink)
		}

	default:
		return errors.Errorf("unsupported feed format.")
	}

	sent := 0
	err = luigi.Pump(ctx, luigiutils.NewSinkCounter(&sent, sink), src)

	// track number of messages sent
	if m.sysCtr != nil {
		m.sysCtr.With("event", "gossiptx").Add(float64(sent))
	} else {
		if sent > 0 {
			level.Debug(feedLogger).Log("event", "gossiptx", "n", sent)
		}
	}

	if errors.Cause(err) == context.Canceled || muxrpc.IsSinkClosed(err) {
		sink.Close()
		return nil
	} else if err != nil {
		return errors.Wrap(err, "failed to pump messages to peer")
	}

	// cryptix: this seems to produce some hangs
	// TODO: make tests with leaving and joining peers while messages are published
	if arg.Live {
		return m.addLiveFeed(
			ctx, sink,
			arg.ID.Ref(),
			latest,
			liveLimit(arg, latest),
		)
	}
	return sink.Close()
}
