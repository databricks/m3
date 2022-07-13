// Copyright (c) 2020 Uber Technologies, Inc.
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

package prometheus

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	promstorage "github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/uber-go/tally"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/m3db/m3/src/query/block"
	"github.com/m3db/m3/src/query/generated/proto/prompb"
	"github.com/m3db/m3/src/query/models"
	"github.com/m3db/m3/src/query/parser/promql"
	"github.com/m3db/m3/src/query/storage"
	"github.com/m3db/m3/src/query/storage/cache"
	"github.com/m3db/m3/src/x/instrument"
)

type prometheusQueryable struct {
	storage storage.Storage
	scope   tally.Scope
	logger  *zap.Logger
	cache   *cache.Cache
}

// PrometheusOptions are options to create a prometheus queryable backed by
// a m3 storage.
type PrometheusOptions struct {
	Storage           storage.Storage
	InstrumentOptions instrument.Options
	CacheOptions      *cache.CacheOptions
}

// StorageErr wraps all errors returned by the storage layer.
// This allows the http handlers that call the Prometheus library directly to distinguish prometheus library errors
// and remote storage errors.
type StorageErr struct {
	inner error
}

// NewStorageErr wraps the provided error as a StorageErr.
func NewStorageErr(err error) *StorageErr {
	return &StorageErr{inner: err}
}

// Unwrap returns the underlying error.
func (e *StorageErr) Unwrap() error {
	return e.inner
}

// Error returns the error string for the underlying error.
func (e *StorageErr) Error() string {
	return e.inner.Error()
}

// NewPrometheusQueryable returns a new prometheus queryable backed by a m3
// storage.
func NewPrometheusQueryable(opts PrometheusOptions) promstorage.Queryable {
	scope := opts.InstrumentOptions.MetricsScope().Tagged(map[string]string{"storage": "prometheus_storage"})
	return &prometheusQueryable{
		storage: opts.Storage,
		scope:   scope,
		logger:  opts.InstrumentOptions.Logger(),
		cache:   cache.NewCache(opts.CacheOptions),
	}
}

// Querier returns a prometheus storage Querier.
func (q *prometheusQueryable) Querier(
	ctx context.Context, _, _ int64,
) (promstorage.Querier, error) {
	return newQuerier(ctx, q.storage, q.logger, q.cache), nil
}

type querier struct {
	ctx     context.Context
	storage storage.Storage
	logger  *zap.Logger
	cache   *cache.Cache
}

func newQuerier(
	ctx context.Context,
	storage storage.Storage,
	logger *zap.Logger,
	cache *cache.Cache,
) promstorage.Querier {
	return &querier{
		ctx:     ctx,
		storage: storage,
		logger:  logger,
		cache:   cache,
	}
}

func (q *querier) Select(
	sortSeries bool,
	hints *promstorage.SelectHints,
	labelMatchers ...*labels.Matcher,
) promstorage.SeriesSet {
	start := time.Now()
	matchers, err := promql.LabelMatchersToModelMatcher(labelMatchers, models.NewTagOptions())
	if err != nil {
		return promstorage.ErrSeriesSet(err)
	}

	query := &storage.FetchQuery{
		TagMatchers: matchers,
		Start:       time.Unix(0, hints.Start*int64(time.Millisecond)),
		End:         time.Unix(0, hints.End*int64(time.Millisecond)),
		Interval:    time.Duration(hints.Step) * time.Millisecond,
	}

	res := make([]string, len(matchers))
	for i, m := range matchers {
		res[i] = m.String()
	}
	sort.Strings(res)

	label_key := strings.Join(res, ";")

	q.logger.Info("fetch query", zap.String("key", label_key), zap.String("metric", res[0]), zap.Duration("range", query.End.Sub(query.Start)))

	// NB (@shreyas): The fetch options builder sets it up from the request
	// which we do not have access to here.
	fetchOptions, err := fetchOptions(q.ctx)
	if err != nil {
		q.logger.Error("fetch options not provided in context", zap.Error(err))
		return promstorage.ErrSeriesSet(err)
	}

	result, err := q.storage.FetchProm(q.ctx, query, fetchOptions)
	// result, err := q.cache.GetValue(query, label_key, q.ctx, q.storage, fetchOptions, q.logger)
	if err != nil {
		return promstorage.ErrSeriesSet(NewStorageErr(err))
	}
	seriesSet := fromQueryResult(sortSeries, result.PromResult, result.Metadata)

	receiveResultMetadataFn, err := resultMetadataReceiveFn(q.ctx)
	if err != nil {
		q.logger.Error("result metadata not set in context", zap.Error(err))
		return promstorage.ErrSeriesSet(err)
	}
	if receiveResultMetadataFn == nil {
		err := errors.New("result metadata receive function nil for context")
		q.logger.Error(err.Error())
		return promstorage.ErrSeriesSet(err)
	}

	// Pass the result.Metadata back using the receive function.
	// This handles concurrent updates to a single result metadata.
	receiveResultMetadataFn(result.Metadata)

	var fields []zapcore.Field

	fields = append(fields, zap.String("key", label_key))
	// if len(result.PromResult.Timeseries) != 0 {
	// 	var tlbls []string
	// 	for _, ts := range result.PromResult.Timeseries {
	// 		var temps []string
	// 		for _, lbl := range ts.Labels {
	// 			temps = append(temps, lbl.String())
	// 		}
	// 		tlbls = append(tlbls, strings.Join(temps, ";"))
	// 	}
	// 	// fields = append(fields, zap.Int("timeseries_cnt", len(result.PromResult.Timeseries)), zap.Ints("sample_cnts", lengths), zap.Int("est_bytes", result.Metadata.FetchedBytesEstimate))
	// 	fields = append(fields, zap.Strings("labels", tlbls))
	// }
	// json.Marshal(result)
	// fields = append(fields, zap.String("prom_result", string(e)), zap.Int("prom_result_len", len(string(e))))
	fields = append(fields, zap.Duration("elapse", time.Since(start)))
	q.logger.Info("fetch response", fields...)

	return seriesSet
}

func (q *querier) LabelValues(string, ...*labels.Matcher) ([]string, promstorage.Warnings, error) {
	// TODO (@shreyas): Implement this.
	q.logger.Warn("calling unsupported LabelValues method")
	return nil, nil, errors.New("not implemented")
}

func (q *querier) LabelNames(...*labels.Matcher) ([]string, promstorage.Warnings, error) {
	// TODO (@shreyas): Implement this.
	q.logger.Warn("calling unsupported LabelNames method")
	return nil, nil, errors.New("not implemented")
}

func (q *querier) Close() error {
	return nil
}

func fromWarningStrings(warnings []string) []error {
	errs := make([]error, 0, len(warnings))
	for _, warning := range warnings {
		errs = append(errs, errors.New(warning))
	}
	return errs
}

// This is a copy of the prometheus remote.FromQueryResult method. Need to
// copy so that this can understand m3 prompb struct.
func fromQueryResult(sortSeries bool, res *prompb.QueryResult, metadata block.ResultMetadata) promstorage.SeriesSet {
	series := make([]promstorage.Series, 0, len(res.Timeseries))
	for _, ts := range res.Timeseries {
		labels := labelProtosToLabels(ts.Labels)
		if err := validateLabelsAndMetricName(labels); err != nil {
			return promstorage.ErrSeriesSet(err)
		}

		series = append(series, &concreteSeries{
			labels:  labels,
			samples: ts.Samples,
		})
	}

	if sortSeries {
		sort.Sort(byLabel(series))
	}

	warnings := fromWarningStrings(metadata.WarningStrings())

	return &concreteSeriesSet{
		series:   series,
		warnings: warnings,
	}
}

type byLabel []promstorage.Series

func (a byLabel) Len() int           { return len(a) }
func (a byLabel) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byLabel) Less(i, j int) bool { return labels.Compare(a[i].Labels(), a[j].Labels()) < 0 }

func labelProtosToLabels(labelPairs []prompb.Label) labels.Labels {
	result := make(labels.Labels, 0, len(labelPairs))
	for _, l := range labelPairs {
		result = append(result, labels.Label{
			Name:  string(l.Name),
			Value: string(l.Value),
		})
	}
	sort.Sort(result)
	return result
}

// errSeriesSet implements storage.SeriesSet, just returning an error.
type errSeriesSet struct {
	err error
}

func (errSeriesSet) Next() bool {
	return false
}

func (errSeriesSet) At() promstorage.Series {
	return nil
}

func (e errSeriesSet) Err() error {
	return e.err
}

// concreteSeriesSet implements storage.SeriesSet.
type concreteSeriesSet struct {
	cur      int
	series   []promstorage.Series
	warnings promstorage.Warnings
}

func (c *concreteSeriesSet) Next() bool {
	c.cur++
	return c.cur-1 < len(c.series)
}

func (c *concreteSeriesSet) At() promstorage.Series {
	return c.series[c.cur-1]
}

func (c *concreteSeriesSet) Err() error {
	return nil
}

func (c *concreteSeriesSet) Warnings() promstorage.Warnings {
	return c.warnings
}

// concreteSeries implements storage.Series.
type concreteSeries struct {
	labels  labels.Labels
	samples []prompb.Sample
}

func (c *concreteSeries) Labels() labels.Labels {
	return labels.New(c.labels...)
}

func (c *concreteSeries) Iterator() chunkenc.Iterator {
	return newConcreteSeriersIterator(c)
}

// concreteSeriesIterator implements storage.SeriesIterator.
type concreteSeriesIterator struct {
	cur    int
	series *concreteSeries
}

func newConcreteSeriersIterator(series *concreteSeries) chunkenc.Iterator {
	return &concreteSeriesIterator{
		cur:    -1,
		series: series,
	}
}

// Seek implements storage.SeriesIterator.
func (c *concreteSeriesIterator) Seek(t int64) bool {
	c.cur = sort.Search(len(c.series.samples), func(n int) bool {
		return c.series.samples[n].Timestamp >= t
	})
	return c.cur < len(c.series.samples)
}

// At implements storage.SeriesIterator.
func (c *concreteSeriesIterator) At() (t int64, v float64) {
	s := c.series.samples[c.cur]
	return s.Timestamp, s.Value
}

// Next implements storage.SeriesIterator.
func (c *concreteSeriesIterator) Next() bool {
	c.cur++
	return c.cur < len(c.series.samples)
}

// Err implements storage.SeriesIterator.
func (c *concreteSeriesIterator) Err() error {
	return nil
}

// validateLabelsAndMetricName validates the label names/values and metric names returned from remote read,
// also making sure that there are no labels with duplicate names
func validateLabelsAndMetricName(ls labels.Labels) error {
	for i, l := range ls {
		if l.Name == labels.MetricName && !model.IsValidMetricName(model.LabelValue(l.Value)) {
			return errors.Errorf("invalid metric name: %v", l.Value)
		}
		if !model.LabelName(l.Name).IsValid() {
			return errors.Errorf("invalid label name: %v", l.Name)
		}
		if !model.LabelValue(l.Value).IsValid() {
			return errors.Errorf("invalid label value: %v", l.Value)
		}
		if i > 0 && l.Name == ls[i-1].Name {
			return errors.Errorf("duplicate label with name: %v", l.Name)
		}
	}
	return nil
}

// type StoreKey struct {
// 	Labels string
// 	Range  int64
// }

// type StoreVal struct {
// 	Result storage.PromResult
// 	Minute int64
// }

// type SimpleCache struct {
// 	Store map[StoreKey]StoreVal
// 	mtx   sync.Mutex
// }

// func NewSimpleCache() *SimpleCache {
// 	return &SimpleCache{
// 		Store: make(map[StoreKey]StoreVal),
// 	}
// }

// func (cache *SimpleCache) get(key StoreKey) (StoreVal, bool) {
// 	cache.mtx.Lock()
// 	if val, ok := cache.Store[key]; ok {
// 		cache.mtx.Unlock()
// 		return val, true
// 	}
// 	cache.mtx.Unlock()
// 	return StoreVal{}, false
// }

// func (cache *SimpleCache) set(key StoreKey, val StoreVal) bool {
// 	cache.mtx.Lock()

// 	cache.Store[key] = val

// 	cache.mtx.Unlock()
// 	return true
// }

// func (cache *SimpleCache) GetValue(
// 	q *storage.FetchQuery,
// 	label_key string,
// 	ctx context.Context,
// 	st storage.Storage,
// 	fetchOptions *storage.FetchOptions,
// 	log *zap.Logger) (storage.PromResult, error) {
// 	queryRange := int64(q.End.Sub(q.Start).Seconds())
// 	key := StoreKey{
// 		Labels: label_key,
// 		Range:  queryRange,
// 	}

// 	minute := int64(q.Start.Minute())

// 	val, ok := cache.get(key)
// 	if ok {
// 		if minute == val.Minute {
// 			length := 0
// 			if len(val.Result.PromResult.Timeseries) != 0 {
// 				for _, ts := range val.Result.PromResult.Timeseries {
// 					length += len(ts.Samples)
// 				}
// 			}
// 			log.Info("cache hit", zap.Int64("bytes", int64(val.Result.Metadata.FetchedBytesEstimate)), zap.Int("num_samples", length))
// 			return val.Result, nil
// 		}
// 	}

// 	result, err := st.FetchProm(ctx, q, fetchOptions)
// 	length := 0
// 	if len(result.PromResult.Timeseries) != 0 {
// 		for _, ts := range result.PromResult.Timeseries {
// 			length += len(ts.Samples)
// 		}
// 	}
// 	log.Info("cache miss", zap.Int64("minute", minute), zap.String("label_key", label_key), zap.Int64("range", queryRange), zap.Int64("bytes", int64(result.Metadata.FetchedBytesEstimate)), zap.Int("num_samples", length))

// 	val = StoreVal{
// 		Result: result,
// 		Minute: minute,
// 	}
// 	if err == nil {
// 		cache.set(key, val)
// 	}
// 	return result, err
// }
