// Copyright (c) 2021  Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package promremote

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/m3db/m3/src/query/block"
	"github.com/m3db/m3/src/query/models"
	"github.com/m3db/m3/src/query/storage"
	"github.com/m3db/m3/src/query/storage/m3/consolidators"
	"github.com/m3db/m3/src/query/storage/m3/storagemetadata"
	"github.com/m3db/m3/src/query/ts"
	xerrors "github.com/m3db/m3/src/x/errors"
	"github.com/m3db/m3/src/x/instrument"
	xhttp "github.com/m3db/m3/src/x/net/http"
	xsync "github.com/m3db/m3/src/x/sync"

	"github.com/pkg/errors"
	"github.com/uber-go/tally"
	"go.uber.org/zap"
)

const metricsScope = "prom_remote_storage"
const logSamplingRate = 0.001

var errorReadingBody = []byte("error reading body")

// WriteQueue A thread-safe queue
type WriteQueue struct {
	t        tenantKey
	capacity int
	queries  []*storage.WriteQuery

	sync.RWMutex
}

func NewWriteQueue(t tenantKey, capacity int) *WriteQueue {
	return &WriteQueue{
		t:        t,
		capacity: capacity,
		queries:  make([]*storage.WriteQuery, 0, capacity),
	}
}

// This one can only be called with the lock held by the call site.
func (wq *WriteQueue) popUnderLock() []*storage.WriteQuery {
	res := wq.queries
	wq.queries = make([]*storage.WriteQuery, 0, wq.capacity)
	return res
}

func (wq *WriteQueue) pop() []*storage.WriteQuery {
	wq.Lock()
	defer wq.Unlock()
	return wq.popUnderLock()
}

func (wq *WriteQueue) Len() int {
	wq.RLock()
	defer wq.RUnlock()
	return len(wq.queries)
}

func (wq *WriteQueue) Add(query *storage.WriteQuery) []*storage.WriteQuery {
	wq.Lock()
	defer wq.Unlock()
	// We can probably optimize lock contention for the case where the queue is full,
	// but the majority of the time it won't be full and therefore not worth optimizating.
	// NB: we have to check if the queue is full under the lock. Otherwise, two goroutines
	// may see the full queue and try to pop it at the same time.
	var res []*storage.WriteQuery
	if len(wq.queries) >= wq.capacity {
		res = wq.popUnderLock()
	}
	wq.queries = append(wq.queries, query)
	return res
}

func (wq *WriteQueue) Flush(ctx context.Context, p *promStorage) {
	data := wq.pop()
	size := int64(len(data))
	if size == 0 {
		return
	}
	p.tickWrites.Inc(1)
	if err := p.writeBatch(ctx, wq.t, data); err != nil {
		p.logger.Error("error writing async batch",
			zap.String("tenant", string(wq.t)),
			zap.Error(err))
	}
}

// introduce a dead letter queue to store the timed out samples from main queue
// samples inside the dead letter queue are flushed to the remote endpoint at next tick
// if dead letter queue is full, samples will be dropped to avoid cascading failures and retry storms
type deadLetterQueue struct {
	capacity int
	queries  []*storage.WriteQuery

	sync.Mutex
}

func newDeadLetterQueue(logger *zap.Logger, capacity int) *deadLetterQueue {
	logger.Info("Creating a new dead letter queue", zap.Int("capacity", capacity))
	return &deadLetterQueue{
		capacity: capacity,
		queries:  make([]*storage.WriteQuery, 0, capacity),
	}
}

func (dlq *deadLetterQueue) size() int {
	dlq.Lock()
	defer dlq.Unlock()
	return len(dlq.queries)
}

func (dlq *deadLetterQueue) add(query *storage.WriteQuery) error {
	if query == nil {
		return nil
	}
	dlq.Lock()
	defer dlq.Unlock()
	if len(dlq.queries) < dlq.capacity {
		dlq.queries = append(dlq.queries, query)
		return nil
	} else {
		return errors.New("dead letter queue is full")
	}
}

func (dlq *deadLetterQueue) flush(p *promStorage, ctx context.Context, wg *sync.WaitGroup, pendingQuery map[tenantKey]*WriteQueue) {
	dlq.Lock()
	defer dlq.Unlock()
	p.dlqSize.Update(float64(len(dlq.queries)))
	for _, query := range dlq.queries {
		p.appendSample(ctx, wg, pendingQuery, query)
	}
	dlq.queries = dlq.queries[:0] // empty the queue
}

func validateOptions(opts Options) error {
	if opts.poolSize < 1 {
		return errors.New("poolSize must be greater than 0")
	}
	if opts.queueSize < 1 {
		return errors.New("queueSize must be greater than 0 to batch writes")
	}
	if opts.retries < 0 {
		return errors.New("retries must be greater than or equal to 0")
	}
	if opts.tickDuration == nil {
		return errors.New("tickDuration must be set")
	}
	if len(opts.endpoints) == 0 {
		return errors.New("endpoint must not be empty")
	}
	return nil
}

// NewStorage returns new Prometheus remote write compatible storage
func NewStorage(opts Options) (storage.Storage, error) {
	if err := validateOptions(opts); err != nil {
		return nil, err
	}
	opts.logger.Info("Creating a new promoremote storage...")
	client := xhttp.NewHTTPClient(opts.httpOptions)
	scope := opts.scope.SubScope(metricsScope)
	// Use fixed
	queriesWithFixedTenants := make(map[tenantKey]*WriteQueue, len(opts.tenantRules)+1)
	queriesWithFixedTenants[tenantKey(opts.tenantDefault)] = NewWriteQueue(tenantKey(opts.tenantDefault), opts.queueSize)
	for _, rule := range opts.tenantRules {
		tenant := tenantKey(rule.Tenant)
		if _, ok := queriesWithFixedTenants[tenant]; !ok {
			opts.logger.Info("Added a new tenant to the fixed tenant list", zap.String("tenant", string(tenant)))
			queriesWithFixedTenants[tenant] = NewWriteQueue(tenant, opts.queueSize)
		}
	}
	// large data queue size to avoid dropping samples
	dataQueueCapacity := (opts.retries + 1) * len(opts.tenantRules) * opts.queueSize
	opts.logger.Info("Creating data queue", zap.Int("capacity", dataQueueCapacity))
	s := &promStorage{
		opts:            opts,
		client:          client,
		endpointMetrics: initEndpointMetrics(opts.endpoints, scope),
		scope:           scope,
		enqueuedSamples: scope.Counter("enqueued_samples"),
		writtenSamples:  scope.Counter("written_samples"),
		droppedSamples:  scope.Counter("dropped_samples"),
		failedSamples:   scope.Counter("failed_samples"),
		inFlightSamples: scope.Gauge("in_flight_samples"),
		batchWrites:     scope.Counter("batch_writes"),
		tickWrites:      scope.Counter("tick_writes"),
		droppedWrites:   scope.Counter("dropped_writes"),
		errWrites:       scope.Counter("err_writes"),
		retryWrites:     scope.Counter("retry_writes"),
		dupWrites:       scope.Counter("duplicate_writes"),
		logger:          opts.logger,
		dataQueue:       make(chan *storage.WriteQuery, dataQueueCapacity),
		dataQueueSize:   scope.Gauge("data_queue_size"),
		dlq:             newDeadLetterQueue(opts.logger, dataQueueCapacity),
		dlqSize:         scope.Gauge("dead_letter_queue_size"),
		workerPool:      xsync.NewWorkerPool(opts.poolSize),
		writeLoopDone:   make(chan struct{}),
	}
	// carry over this queriesWithFixedTenants to make sure it is not concurrency safe
	s.startAsync(queriesWithFixedTenants)
	opts.logger.Info("Prometheus remote write storage created", zap.Int("num_tenants", len(queriesWithFixedTenants)))
	return s, nil
}

type promStorage struct {
	unimplementedPromStorageMethods
	opts            Options
	client          *http.Client
	endpointMetrics map[string]*instrument.HttpMetrics
	scope           tally.Scope
	// Don't measure WriteQuery it is a very weird M3 internal data structure.
	// samples are # of data points inside each WriteQuery
	enqueuedSamples     tally.Counter
	writtenSamples      tally.Counter
	droppedSamples      tally.Counter
	failedSamples       tally.Counter
	inFlightSamples     tally.Gauge
	inFlightSampleValue atomic.Int64
	// writes are # of http requests to downstream remote endpoints
	tickWrites    tally.Counter
	batchWrites   tally.Counter
	droppedWrites tally.Counter
	errWrites     tally.Counter
	retryWrites   tally.Counter
	dupWrites     tally.Counter
	logger        *zap.Logger
	dataQueue     chan *storage.WriteQuery
	dataQueueSize tally.Gauge
	dlq           *deadLetterQueue
	dlqSize       tally.Gauge
	workerPool    xsync.WorkerPool
	writeLoopDone chan struct{}
}

type tenantKey string

func (p *promStorage) getTenant(query *storage.WriteQuery) tenantKey {
	for _, rule := range p.opts.tenantRules {
		if ok := rule.Filter.MatchTags(query.Tags()); ok {
			return tenantKey(rule.Tenant)
		}
	}
	return tenantKey(p.opts.tenantDefault)
}

func (p *promStorage) appendSample(ctx context.Context, wg *sync.WaitGroup, pendingQuery map[tenantKey]*WriteQueue, query *storage.WriteQuery) {
	t := p.getTenant(query)
	if _, ok := pendingQuery[t]; !ok {
		p.droppedWrites.Inc(1)
		p.logger.Error("no pre-defined tenant found, dropping it",
			zap.String("tenant", string(t)),
			zap.String("defaultTenant", p.opts.tenantDefault),
			zap.String("timeseries", query.String()))
		return
	}
	if dataBatch := pendingQuery[t].Add(query); dataBatch != nil {
		p.batchWrites.Inc(1)
		wg.Add(1)
		p.workerPool.Go(func() {
			defer wg.Done()
			if err := p.writeBatch(ctx, t, dataBatch); err != nil {
				p.logger.Error("error writing async batch",
					zap.String("tenant", string(t)),
					zap.Error(err))
			}
		})
	}
}

func (p *promStorage) flushPendingQueues(ctx context.Context, wg *sync.WaitGroup, pendingQuery map[tenantKey]*WriteQueue) int {
	numWrites := 0
	p.dlq.flush(p, ctx, wg, pendingQuery)
	for _, queue := range pendingQuery {
		if queue.Len() == 0 {
			continue
		}
		numWrites += queue.Len()
		wg.Add(1)
		// Copy the loop variable
		q := queue
		p.workerPool.Go(func() {
			q.Flush(ctx, p)
			wg.Done()
		})
	}
	return numWrites
}

func (p *promStorage) writeLoop(pendingQuery map[tenantKey]*WriteQueue) {
	// This function ensures that all pending writes are flushed before returning.
	ctxForWrites, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	p.workerPool.Init()
	ticker := time.NewTicker(*p.opts.tickDuration)
	stop := false
	for !stop {
		select {
		case query := <-p.dataQueue:
			if query == nil {
				p.logger.Info("Got the poison pill. Exiting the write loop.")
				// The channel is closed. We should exit.
				stop = true
				// This breaks out select instead of the for loop.
				break
			}
			p.appendSample(ctxForWrites, &wg, pendingQuery, query)
			break
		case <-ticker.C:
			p.flushPendingQueues(ctxForWrites, &wg, pendingQuery)
		}
	}
	// At this point, `p.dataQueue` is drained and closed.
	p.logger.Info("Draining pending per-tenant write queues")
	numWrites := p.flushPendingQueues(ctxForWrites, &wg, pendingQuery)
	p.logger.Info("Waiting for all async pending writes to finish",
		zap.Int("numWrites", numWrites))
	// Block until all pending writes are flushed because we don't want to lose any data.
	wg.Wait()
	p.logger.Info("All async pending writes are done",
		zap.Int("numWrites", numWrites))
	p.writeLoopDone <- struct{}{}
}

func (p *promStorage) startAsync(pendingQuery map[tenantKey]*WriteQueue) {
	p.logger.Info("Start prometheus remote write storage async job",
		zap.Int("queueSize", p.opts.queueSize),
		zap.Int("poolSize", p.opts.poolSize))
	go func() {
		p.logger.Info("Starting the write loop")
		p.writeLoop(pendingQuery)
	}()
}

func deepCopy(queryOpt storage.WriteQueryOptions) storage.WriteQueryOptions {
	// Only need Tags and DataPoints for writing to remote Prom. Other field are not used.
	// getTenant() only uses Tags.Tags.
	// See src/query/storage/promremote/query_coverter.go
	// Unit is copied to pass the validation in NewWriteQuery()
	// FromIngestor is used for logging only.
	cp := storage.WriteQueryOptions{
		Unit: queryOpt.Unit,
		Tags: models.Tags{
			Opts: queryOpt.Tags.Opts,
		},
		FromIngestor: queryOpt.FromIngestor,
	}
	cp.Datapoints = make([]ts.Datapoint, 0, len(queryOpt.Datapoints))
	cp.Datapoints = append(cp.Datapoints, queryOpt.Datapoints...)

	cp.Tags.Tags = make([]models.Tag, 0, len(queryOpt.Tags.Tags))
	cp.Tags.Tags = append(cp.Tags.Tags, queryOpt.Tags.Tags...)
	/*
		// In case deeper copying is needed
		for i, tag := range queryOpt.Tags.Tags {
			tagCopy := models.Tag{
				Name:  make([]byte, len(tag.Name)),
				Value:  make([]byte, len(tag.Value)),
			}
			copy(tagCopy.Name, tag.Name)
			copy(tagCopy.Value, tag.Value)
			cp.Tags.Tags[i] = tagCopy
		}
	*/
	return cp
}

func (p *promStorage) Write(_ context.Context, query *storage.WriteQuery) error {
	if query == nil {
		return nil
	}
	samples := int64(query.Datapoints().Len())
	if query.Options().DuplicateWrite {
		// M3 call site may write the same data according to different storage policies.
		// See downsampleAndWriter in src/cmd/services/m3coordinator/ingest/write.go
		p.dupWrites.Inc(1)
		return nil
	}
	if query.Options().FromIngestor {
		// src/cmd/services/m3coordinator/ingest/m3msg/ingest.go reuses a WriteQuery object to write different
		// time series by calling ResetWriteQuery(). We need to make a copy of the WriteQuery object to avoid
		// race conditions.
		queryCopy, err := storage.NewWriteQuery(deepCopy(query.Options()))
		if err != nil {
			p.droppedSamples.Inc(samples)
			p.logger.Error("error copying write", zap.Error(err), zap.String("write", query.String()))
			return nil
		}
		query = queryCopy
	}

	select {
	case p.dataQueue <- query:
		// The data is enqueued successfully.
		p.enqueuedSamples.Inc(samples)
		p.inFlightSamples.Update(float64(p.inFlightSampleValue.Add(samples)))
		p.dataQueueSize.Update(float64(len(p.dataQueue)))
	case <-time.After(*p.opts.queueTimeout):
		err := p.dlq.add(query)
		if err != nil {
			p.droppedSamples.Inc(samples)
			if rand.Float32() < logSamplingRate {
				p.logger.Error("error enqueue samples for prom remote write", zap.Error(err),
					zap.String("data", query.String()))
			}
		}
	}
	return nil
}

func (p *promStorage) writeBatch(ctx context.Context, tenant tenantKey, queries []*storage.WriteQuery) error {
	if rand.Float32() < logSamplingRate {
		p.logger.Debug("async write batch",
			zap.String("tenant", string(tenant)),
			zap.Int("size", len(queries)))
	}
	if len(queries) == 0 {
		return nil
	}
	encoded, samples, err := convertAndEncodeWriteQuery(queries)
	sampleCount := int64(samples)
	p.logger.Debug("async write batch",
		zap.String("tenant", string(tenant)),
		zap.Int("size", len(queries)), zap.Int64("samples", sampleCount))
	p.inFlightSamples.Update(float64(p.inFlightSampleValue.Add(-sampleCount)))
	if err != nil {
		p.errWrites.Inc(1)
		p.failedSamples.Inc(sampleCount)
		return err
	}

	// We only write to the first endpoint since this storage(Panthoen) doesn't distinguish raw data samples
	// from aggregated ones.
	endpoint := p.opts.endpoints[0]
	metrics := p.endpointMetrics[endpoint.name]
	err = p.write(ctx, metrics, endpoint, tenant, bytes.NewReader(encoded))
	if err != nil {
		p.errWrites.Inc(1)
		p.failedSamples.Inc(sampleCount)
	} else {
		p.writtenSamples.Inc(sampleCount)
	}
	return err
}

func (p *promStorage) Type() storage.Type {
	return storage.TypeRemoteDC
}

func (p *promStorage) Close() error {
	close(p.dataQueue)
	p.logger.Info("Closing prometheus remote write storage",
		zap.String("remote store", p.Name()),
		zap.Int("data queue size", len(p.dataQueue)))
	// Blocked until all pending writes are flushed.
	<-p.writeLoopDone
	p.dataQueueSize.Update(float64(len(p.dataQueue)))
	// After this point, all writes are flushed or errored out.
	p.client.CloseIdleConnections()
	return nil
}

func (p *promStorage) ErrorBehavior() storage.ErrorBehavior {
	return storage.BehaviorFail
}

func (p *promStorage) Name() string {
	return "prom-remote"
}

// The actual method to write to remote endpoint
func (p *promStorage) write(
	ctx context.Context,
	metrics *instrument.HttpMetrics,
	endpoint EndpointOptions,
	tenant tenantKey,
	encoded io.Reader,
) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.address, encoded)
	if err != nil {
		return err
	}
	req.Header.Set("content-encoding", "snappy")
	req.Header.Set(xhttp.HeaderContentType, xhttp.ContentTypeProtobuf)
	if endpoint.apiToken != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Basic %s",
			base64.StdEncoding.EncodeToString([]byte(
				fmt.Sprintf("%s:%s", string(tenant), endpoint.apiToken),
			)),
		))
	}
	if len(endpoint.otherHeaders) > 0 {
		for k, v := range endpoint.otherHeaders {
			// set headers defined in remote endpoint options
			req.Header.Set(k, v)
		}
	}
	req.Header.Set(endpoint.tenantHeader, string(tenant))

	start := time.Now()
	status := 0
	backoff := 100 * time.Millisecond
	for i := p.opts.retries; i >= 0; i-- {
		status, err = p.doRequest(req)
		if err == nil || status == http.StatusConflict || status == http.StatusTooManyRequests {
			// 409 is a valid status code due to RWA dual scrape issue
			// see https://docs.google.com/document/d/19exXqcXxtc37jbdFbztt97-I2S5A873__sAMOGFWD6Q/edit?tab=t.0#heading=h.8kznn96p9jea
			// we don't want to retry on 429 if the tenant is already over the active series limit for cascading failures
			err = nil
			break
		}
		p.retryWrites.Inc(1)
		time.Sleep(backoff)
		backoff *= 2
	}
	methodDuration := time.Since(start)
	metrics.RecordResponse(status, methodDuration)
	return err
}

func (p *promStorage) doRequest(req *http.Request) (int, error) {
	resp, err := p.client.Do(req)
	if err != nil {
		return http.StatusServiceUnavailable, fmt.Errorf("503 error to connect to remote endpoint: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		response, err := io.ReadAll(resp.Body)
		if err != nil {
			p.logger.Error("error reading body", zap.Error(err))
			response = errorReadingBody
		}
		genericError := fmt.Errorf("expected status code 2XX: actual=%v,  resp=%s", resp.StatusCode, response)
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return resp.StatusCode, xerrors.NewInvalidParamsError(genericError)
		}
		return resp.StatusCode, genericError
	}
	return resp.StatusCode, nil
}

func initEndpointMetrics(endpoints []EndpointOptions, scope tally.Scope) map[string]*instrument.HttpMetrics {
	metrics := make(map[string]*instrument.HttpMetrics, len(endpoints))
	for _, endpoint := range endpoints {
		endpointScope := scope.Tagged(map[string]string{"endpoint_name": endpoint.name})
		httpMetrics := instrument.NewHttpMetrics(endpointScope, "write", instrument.TimerOptions{
			Type:             instrument.HistogramTimerType,
			HistogramBuckets: tally.DefaultBuckets,
		})
		metrics[endpoint.name] = httpMetrics
	}
	return metrics
}

var _ storage.Storage = &promStorage{}

type unimplementedPromStorageMethods struct{}

func (p *unimplementedPromStorageMethods) FetchProm(
	_ context.Context,
	_ *storage.FetchQuery,
	_ *storage.FetchOptions,
) (storage.PromResult, error) {
	return storage.PromResult{}, unimplementedError("FetchProm")
}

func (p *unimplementedPromStorageMethods) FetchBlocks(
	_ context.Context,
	_ *storage.FetchQuery,
	_ *storage.FetchOptions,
) (block.Result, error) {
	return block.Result{}, unimplementedError("FetchBlocks")
}

func (p *unimplementedPromStorageMethods) FetchCompressed(
	_ context.Context,
	_ *storage.FetchQuery,
	_ *storage.FetchOptions,
) (consolidators.MultiFetchResult, error) {
	return nil, unimplementedError("FetchCompressed")
}

func (p *unimplementedPromStorageMethods) SearchSeries(
	_ context.Context,
	_ *storage.FetchQuery,
	_ *storage.FetchOptions,
) (*storage.SearchResults, error) {
	return nil, unimplementedError("SearchSeries")
}

func (p *unimplementedPromStorageMethods) CompleteTags(
	_ context.Context,
	_ *storage.CompleteTagsQuery,
	_ *storage.FetchOptions,
) (*consolidators.CompleteTagsResult, error) {
	return nil, unimplementedError("CompleteTags")
}

func (p *unimplementedPromStorageMethods) QueryStorageMetadataAttributes(
	_ context.Context,
	_, _ time.Time,
	_ *storage.FetchOptions,
) ([]storagemetadata.Attributes, error) {
	return nil, unimplementedError("QueryStorageMetadataAttributes")
}

func unimplementedError(name string) error {
	return fmt.Errorf("promStorage: %s method is not supported", name)
}
