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
	if len(wq.queries) >= wq.capacity {
		return wq.popUnderLock()
	}
	wq.queries = append(wq.queries, query)
	return nil
}

func (wq *WriteQueue) Flush(ctx context.Context, p *promStorage) {
	data := wq.pop()
	size := int64(len(data))
	if size == 0 {
		return
	}
	p.tickWrite.Inc(size)
	if err := p.writeBatch(ctx, wq.t, data); err != nil {
		p.logger.Error("error writing async batch",
			zap.String("tenant", string(wq.t)),
			zap.Error(err))
	}
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
	ctx, cancel := context.WithCancel(context.Background())
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
	s := &promStorage{
		opts:            opts,
		client:          client,
		endpointMetrics: initEndpointMetrics(opts.endpoints, scope),
		scope:           scope,
		droppedWrites:   scope.Counter("dropped_writes"),
		dupWrites:       scope.Counter("duplicate_writes"),
		ingestorWrites:  scope.Counter("ingestor_writes"),
		enqueued:        scope.Counter("enqueued"),
		enqueueErr:      scope.Counter("enqueue_error"),
		batchWrite:      scope.Counter("batch_write"),
		batchWriteErr:   scope.Counter("batch_write_err"),
		tickWrite:       scope.Counter("tick_write"),
		logger:          opts.logger,
		queryQueue:      make(chan *storage.WriteQuery, opts.queueSize),
		workerPool:      xsync.NewWorkerPool(opts.poolSize),
		pendingQuery:    queriesWithFixedTenants,
		noTenantFound:   scope.Counter("no_tenant_found"),
		cancel:          cancel,
		writeLoopDone:   make(chan struct{}),
		wrongTenant:     scope.Counter("wrong_tenant"),
	}
	s.startAsync(ctx)
	opts.logger.Info("Prometheus remote write storage created", zap.Int("num_tenants", len(queriesWithFixedTenants)))
	return s, nil
}

type promStorage struct {
	unimplementedPromStorageMethods
	opts            Options
	client          *http.Client
	endpointMetrics map[string]*instrument.HttpMetrics
	scope           tally.Scope
	droppedWrites   tally.Counter
	enqueued        tally.Counter
	enqueueErr      tally.Counter
	dupWrites       tally.Counter
	ingestorWrites  tally.Counter
	batchWrite      tally.Counter
	batchWriteErr   tally.Counter
	tickWrite       tally.Counter
	logger          *zap.Logger
	queryQueue      chan *storage.WriteQuery
	workerPool      xsync.WorkerPool
	pendingQuery    map[tenantKey]*WriteQueue
	noTenantFound   tally.Counter
	cancel          context.CancelFunc
	writeLoopDone   chan struct{}
	wrongTenant     tally.Counter
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

func (p *promStorage) flushPendingQueues(minQueueSizeToFlush int, ctx context.Context, wg *sync.WaitGroup) int {
	numWrites := 0
	for t, queue := range p.pendingQuery {
		if queue.Len() == 0 {
			continue
		}
		if queue.Len() < minQueueSizeToFlush {
			p.logger.Warn("don't do tick flush for small batch",
				zap.String("tenant", string(t)),
				zap.Int("size", queue.Len()),
				zap.Int("queue size", p.opts.queueSize))
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

func (p *promStorage) writeLoop() {
	// This function ensures that all pending writes are flushed before returning.
	ctxForWrites, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	p.workerPool.Init()
	ticker := time.NewTicker(*p.opts.tickDuration)
	stop := false
	for !stop {
		select {
		case query := <-p.queryQueue:
			if query == nil {
				p.logger.Info("Got the poison pill. Exiting the write loop.")
				// The channel is closed. We should exit.
				stop = true
				// This breaks out select instead of the for loop.
				break
			}
			t := p.getTenant(query)
			if _, ok := p.pendingQuery[t]; !ok {
				p.noTenantFound.Inc(1)
				p.droppedWrites.Inc(1)
				p.logger.Error("no pre-defined tenant found, dropping it",
					zap.String("tenant", string(t)),
					zap.String("defaultTenant", p.opts.tenantDefault),
					zap.String("timeseries", query.String()))
				continue
			}
			if dataBatch := p.pendingQuery[t].Add(query); dataBatch != nil {
				p.batchWrite.Inc(int64(len(dataBatch)))
				wg.Add(1)
				p.workerPool.Go(func() {
					defer wg.Done()
					if err := p.writeBatch(ctxForWrites, t, dataBatch); err != nil {
						p.logger.Error("error writing async batch",
							zap.String("tenant", string(t)),
							zap.Error(err))
					}
				})
			}
		case <-ticker.C:
			p.flushPendingQueues(p.opts.queueSize/10, ctxForWrites, &wg)
		}
	}
	// At this point, `p.queryQueue` is drained and closed.
	p.logger.Info("Draining pending per-tenant write queues")
	numWrites := p.flushPendingQueues(0, ctxForWrites, &wg)
	p.logger.Info("Waiting for all async pending writes to finish",
		zap.Int("numWrites", numWrites))
	// Block until all pending writes are flushed because we don't want to lose any data.
	wg.Wait()
	p.logger.Info("All async pending writes are done",
		zap.Int("numWrites", numWrites))
	p.writeLoopDone <- struct{}{}
}

func (p *promStorage) startAsync(_ context.Context) {
	p.logger.Info("Start prometheus remote write storage async job",
		zap.Int("queueSize", p.opts.queueSize),
		zap.Int("poolSize", p.opts.poolSize))
	go func() {
		p.logger.Info("Starting the write loop")
		p.writeLoop()
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

func (p *promStorage) Write(ctx context.Context, query *storage.WriteQuery) error {
	if query == nil {
		return nil
	}
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
		p.ingestorWrites.Inc(1)
		queryCopy, err := storage.NewWriteQuery(deepCopy(query.Options()))
		if err != nil {
			p.enqueueErr.Inc(1)
			p.logger.Error("error copying write", zap.Error(err),
				zap.String("write", query.String()))
			return err
		}
		query = queryCopy
	}
	p.queryQueue <- query
	p.enqueued.Inc(1)
	return nil
}

func (p *promStorage) writeBatch(ctx context.Context, tenant tenantKey, queries []*storage.WriteQuery) error {
	logSampling := rand.Float32()
	if logSampling < logSamplingRate {
		p.logger.Debug("async write batch",
			zap.String("tenant", string(tenant)),
			zap.Int("size", len(queries)))
	}
	// TODO: remove this double check once the bug is confirmed to be fixed.
	{
		correctWq := make([]*storage.WriteQuery, 0, len(queries))
		wrongTenants := 0
		for _, wq := range queries {
			correctTenant := p.getTenant(wq)
			if correctTenant == tenant {
				correctWq = append(correctWq, wq)
			} else {
				wrongTenants++
				if rand.Float32() < 0.01 {
					p.logger.Error("dropping a write because of a wrong tenant",
						zap.String("expected_tenant", string(correctTenant)),
						zap.String("actual_tenant", string(tenant)),
						zap.String("time_series", wq.String()),
						zap.Bool("from_ingestor", wq.Options().FromIngestor),
					)
				}
			}
		}
		p.wrongTenant.Inc(int64(wrongTenants))
		queries = correctWq
	}
	if len(queries) == 0 {
		return nil
	}
	encoded, err := convertAndEncodeWriteQuery(queries)
	if err != nil {
		p.batchWriteErr.Inc(1)
		return err
	}

	// We only write to the first endpoint since this storage(Panthoen) doesn't distinguish raw data samples
	// from aggregated ones.
	endpoint := p.opts.endpoints[0]
	metrics := p.endpointMetrics[endpoint.name]
	err = p.write(ctx, metrics, endpoint, tenant, bytes.NewReader(encoded))
	if err != nil {
		p.batchWriteErr.Inc(1)
	}
	return err
}

func (p *promStorage) Type() storage.Type {
	return storage.TypeRemoteDC
}

func (p *promStorage) Close() error {
	close(p.queryQueue)
	p.cancel()
	// Blocked until all pending writes are flushed.
	<-p.writeLoopDone
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
