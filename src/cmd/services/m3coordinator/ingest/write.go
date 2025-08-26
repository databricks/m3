// Copyright (c) 2019 Uber Technologies, Inc.
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

package ingest

import (
	"context"
	"sync"
	"time"

	"github.com/m3db/m3/src/cmd/services/m3coordinator/downsample"
	"github.com/m3db/m3/src/metrics/policy"
	"github.com/m3db/m3/src/query/models"
	"github.com/m3db/m3/src/query/storage"
	"github.com/m3db/m3/src/query/storage/m3/storagemetadata"
	"github.com/m3db/m3/src/query/ts"
	xerrors "github.com/m3db/m3/src/x/errors"
	"github.com/m3db/m3/src/x/instrument"
	xsync "github.com/m3db/m3/src/x/sync"
	xtime "github.com/m3db/m3/src/x/time"

	"github.com/uber-go/tally"
	"go.uber.org/zap"
)

var (
	unaggregatedStoragePolicy   = policy.NewStoragePolicy(0, xtime.Unit(0), 0)
	unaggregatedStoragePolicies = []policy.StoragePolicy{
		unaggregatedStoragePolicy,
	}

	sourceTags = map[ts.SourceType]string{
		ts.SourceTypePrometheus:  "prometheus",
		ts.SourceTypeGraphite:    "graphite",
		ts.SourceTypeOpenMetrics: "open-metrics",
	}
)

// IterValue is the value returned by the iterator.
type IterValue struct {
	Tags       models.Tags
	Datapoints ts.Datapoints
	Attributes ts.SeriesAttributes
	Unit       xtime.Unit
	Metadata   ts.Metadata
	Annotation []byte
}

// DownsampleAndWriteIter is an interface that can be implemented to use
// the WriteBatch method.
type DownsampleAndWriteIter interface {
	Next() bool
	Current() IterValue
	Reset() error
	Error() error
	SetCurrentMetadata(ts.Metadata)
}

// DownsamplerAndWriter is the interface for the downsamplerAndWriter which
// writes metrics to the downsampler as well as to storage in unaggregated form.
type DownsamplerAndWriter interface {
	Write(
		ctx context.Context,
		tags models.Tags,
		datapoints ts.Datapoints,
		unit xtime.Unit,
		annotation []byte,
		overrides WriteOptions,
		source ts.SourceType,
	) error

	WriteBatch(
		ctx context.Context,
		iter DownsampleAndWriteIter,
		overrides WriteOptions,
	) BatchError

	Storage() storage.Storage

	Downsampler() downsample.Downsampler
}

// BatchError allows for access to individual errors.
type BatchError interface {
	error
	Errors() []error
	LastError() error
}

// WriteOptions contains overrides for the downsampling mapping
// rules and storage policies for a given write.
type WriteOptions struct {
	DownsampleMappingRules []downsample.AutoMappingRule
	WriteStoragePolicies   []policy.StoragePolicy

	DownsampleOverride bool
	WriteOverride      bool
}

type downsamplerAndWriterMetrics struct {
	dropped            metricsBySource
	written            metricsBySource
	multiPolicyWritten metricsBySource

	// Latency breakdown histograms
	aggregatedLatency   tally.Histogram
	unaggregatedLatency tally.Histogram
	waitLatency         tally.Histogram
	totalLatency        tally.Histogram
	storeWriteLatency   tally.Histogram
}

type metricsBySource struct {
	bySource  map[ts.SourceType]tally.Counter
	byUnknown tally.Counter
}

func (m metricsBySource) report(source ts.SourceType) {
	counter, ok := m.bySource[source]
	if !ok {
		counter = m.byUnknown
	}
	counter.Inc(1)
}

func (m metricsBySource) reportInc(source ts.SourceType, value int) {
	counter, ok := m.bySource[source]
	if !ok {
		counter = m.byUnknown
	}
	counter.Inc(int64(value))
}

// downsamplerAndWriter encapsulates the logic for writing data to the downsampler,
// as well as in unaggregated form to storage.
type downsamplerAndWriter struct {
	store       storage.Storage
	downsampler downsample.Downsampler
	workerPool  xsync.PooledWorkerPool
	logger      *zap.Logger

	metrics downsamplerAndWriterMetrics
}

// NewDownsamplerAndWriter creates a new downsampler and writer.
func NewDownsamplerAndWriter(
	store storage.Storage,
	downsampler downsample.Downsampler,
	workerPool xsync.PooledWorkerPool,
	instrumentOpts instrument.Options,
) DownsamplerAndWriter {
	scope := instrumentOpts.MetricsScope().SubScope("downsampler")

	// Define histogram buckets for latency (milliseconds)
	latencyBuckets := tally.MustMakeExponentialValueBuckets(1, 2, 20) // 1ms to ~1000s

	return &downsamplerAndWriter{
		store:       store,
		downsampler: downsampler,
		workerPool:  workerPool,
		logger:      instrumentOpts.Logger(),
		metrics: downsamplerAndWriterMetrics{
			dropped:            newMetricsBySource(scope, "metrics_dropped"),
			written:            newMetricsBySource(scope, "metrics_written"),
			multiPolicyWritten: newMetricsBySource(scope, "metrics_multi_policy_written"),

			// Latency breakdown histograms
			aggregatedLatency:   scope.Histogram("write_aggregated_latency_ms", latencyBuckets),
			unaggregatedLatency: scope.Histogram("write_unaggregated_latency_ms", latencyBuckets),
			waitLatency:         scope.Histogram("write_wait_latency_ms", latencyBuckets),
			totalLatency:        scope.Histogram("write_total_latency_ms", latencyBuckets),
			storeWriteLatency:   scope.Histogram("store_write_latency_ms", latencyBuckets),
		},
	}
}

func newMetricsBySource(scope tally.Scope, name string) metricsBySource {
	metrics := metricsBySource{
		bySource:  make(map[ts.SourceType]tally.Counter, len(sourceTags)),
		byUnknown: scope.Tagged(map[string]string{"source": "unknown"}).Counter(name),
	}

	for source, tag := range sourceTags {
		metrics.bySource[source] = scope.Tagged(map[string]string{"source": tag}).Counter(name)
	}

	return metrics
}

func (d *downsamplerAndWriter) Write(
	ctx context.Context,
	tags models.Tags,
	datapoints ts.Datapoints,
	unit xtime.Unit,
	annotation []byte,
	overrides WriteOptions,
	source ts.SourceType,
) error {
	d.logger.Info("Write called", zap.Int("datapoints", len(datapoints)), zap.String("source", sourceTags[source]))

	var (
		multiErr         = xerrors.NewMultiError()
		dropUnaggregated bool
	)

	if d.shouldDownsample(overrides) {
		d.logger.Debug("Write: downsampling enabled")
		var err error
		dropUnaggregated, err = d.writeToDownsampler(tags, datapoints, annotation, overrides)
		if err != nil {
			multiErr = multiErr.Add(err)
		}
	} else {
		d.logger.Debug("Write: downsampling disabled")
	}

	if dropUnaggregated {
		d.logger.Debug("Write: dropping unaggregated data")
		d.metrics.dropped.report(source)
	} else if d.shouldWrite(overrides) {
		d.logger.Debug("Write: writing to storage")
		err := d.writeToStorage(ctx, tags, datapoints, unit, annotation, overrides, source)
		if err != nil {
			multiErr = multiErr.Add(err)
		}
	} else {
		d.logger.Debug("Write: skipping storage write")
	}

	return multiErr.FinalError()
}

func (d *downsamplerAndWriter) shouldWrite(
	overrides WriteOptions,
) bool {
	var (
		// Ensure storage set.
		storageExists = d.store != nil
		// Ensure using default storage policies or some storage policies set.
		useDefaultStoragePolicies = !overrides.WriteOverride
		// If caller tried to override the storage policies, make sure there's
		// at least one.
		_, writeOverride = d.writeOverrideStoragePolicies(overrides)
	)
	// Only write directly to storage if the store exists, and caller wants to
	// use the default storage policies, or they're trying to override the
	// storage policies and they've provided at least one override to do so.
	return storageExists && (useDefaultStoragePolicies || writeOverride)
}

func (d *downsamplerAndWriter) writeOverrideStoragePolicies(
	overrides WriteOptions,
) ([]policy.StoragePolicy, bool) {
	writeOverride := overrides.WriteOverride && len(overrides.WriteStoragePolicies) > 0
	if !writeOverride {
		return nil, false
	}
	return overrides.WriteStoragePolicies, true
}

func (d *downsamplerAndWriter) shouldDownsample(
	overrides WriteOptions,
) bool {
	var (
		// If they didn't request the mapping rules to be overridden, then assume they want the default
		// ones.
		useDefaultMappingRules = !overrides.DownsampleOverride
		// If they did try and override the mapping rules, make sure they've provided at least one.
		_, downsampleOverride = d.downsampleOverrideRules(overrides)
	)
	// Only downsample if the downsampler is enabled, and they either want to use the default mapping
	// rules, or they're trying to override the mapping rules and they've provided at least one
	// override to do so.
	return d.downsampler.Enabled() && (useDefaultMappingRules || downsampleOverride)
}

func (d *downsamplerAndWriter) downsampleOverrideRules(
	overrides WriteOptions,
) ([]downsample.AutoMappingRule, bool) {
	downsampleOverride := overrides.DownsampleOverride && len(overrides.DownsampleMappingRules) > 0
	if !downsampleOverride {
		return nil, false
	}
	return overrides.DownsampleMappingRules, true
}

func (d *downsamplerAndWriter) writeToDownsampler(
	tags models.Tags,
	datapoints ts.Datapoints,
	annotation []byte,
	overrides WriteOptions,
) (bool, error) {
	if err := tags.Validate(); err != nil {
		return false, err
	}

	appender, err := d.downsampler.NewMetricsAppender()
	if err != nil {
		return false, err
	}

	defer appender.Finalize()

	for _, tag := range tags.Tags {
		appender.AddTag(tag.Name, tag.Value)
	}

	if tags.Opts.IDSchemeType() == models.TypeGraphite {
		// NB(r): This is gross, but if this is a graphite metric then
		// we are going to set a special tag that means the downsampler
		// will write a graphite ID. This should really be plumbed
		// through the downsampler in general, but right now the aggregator
		// does not allow context to be attached to a metric so when it calls
		// back the context is lost currently.
		// TODO_FIX_GRAPHITE_TAGGING: Using this string constant to track
		// all places worth fixing this hack. There is at least one
		// other path where flows back to the coordinator from the aggregator
		// and this tag is interpreted, eventually need to handle more cleanly.
		appender.AddTag(downsample.MetricsOptionIDSchemeTagName,
			downsample.GraphiteIDSchemeTagValue)
	}

	// NB: we don't set series attributes on the sample appender options here.
	// In practice this isn't needed because only the carbon ingest path comes through here.
	var appenderOpts downsample.SampleAppenderOptions
	if downsampleMappingRuleOverrides, ok := d.downsampleOverrideRules(overrides); ok {
		appenderOpts = downsample.SampleAppenderOptions{
			Override: true,
			OverrideRules: downsample.SamplesAppenderOverrideRules{
				MappingRules: downsampleMappingRuleOverrides,
			},
		}
	}

	result, err := appender.SamplesAppender(appenderOpts)
	if err != nil {
		return false, err
	}

	for _, dp := range datapoints {
		if result.ShouldDropTimestamp {
			err = result.SamplesAppender.AppendUntimedGaugeSample(dp.Timestamp, dp.Value, annotation)
		} else {
			err = result.SamplesAppender.AppendGaugeSample(
				dp.Timestamp, dp.Value, annotation,
			)
		}
		if err != nil {
			return result.IsDropPolicyApplied, err
		}
	}

	return result.IsDropPolicyApplied, nil
}

func (d *downsamplerAndWriter) writeToStorage(
	ctx context.Context,
	tags models.Tags,
	datapoints ts.Datapoints,
	unit xtime.Unit,
	annotation []byte,
	overrides WriteOptions,
	source ts.SourceType,
) error {
	d.metrics.written.report(source)

	storagePolicies, ok := d.writeOverrideStoragePolicies(overrides)
	if !ok {
		// NB(r): Allocate the write query at the top
		// of the pooled worker instead of need to pass
		// the options down the stack which can cause
		// the stack to grow (and sometimes cause stack splits).
		writeQuery, err := storage.NewWriteQuery(storage.WriteQueryOptions{
			Tags:       tags,
			Datapoints: datapoints,
			Unit:       unit,
			Annotation: annotation,
			Attributes: storageAttributesFromPolicy(unaggregatedStoragePolicy),
		})
		if err != nil {
			return err
		}
		return d.store.Write(ctx, writeQuery)
	}

	var (
		wg       sync.WaitGroup
		multiErr xerrors.MultiError
		errLock  sync.Mutex
	)
	if len(storagePolicies) > 1 {
		d.metrics.multiPolicyWritten.reportInc(source, len(storagePolicies))
	}
	for idx, p := range storagePolicies {
		p := p // Capture for goroutine.
		duplicateWrite := idx > 0

		wg.Add(1)
		d.workerPool.Go(func() {
			// NB(r): Allocate the write query at the top
			// of the pooled worker instead of need to pass
			// the options down the stack which can cause
			// the stack to grow (and sometimes cause stack splits).
			writeQuery, err := storage.NewWriteQuery(storage.WriteQueryOptions{
				Tags:           tags,
				Datapoints:     datapoints,
				Unit:           unit,
				Annotation:     annotation,
				Attributes:     storageAttributesFromPolicy(p),
				DuplicateWrite: duplicateWrite,
			})
			if err == nil {
				err = d.store.Write(ctx, writeQuery)
			}
			if err != nil {
				errLock.Lock()
				multiErr = multiErr.Add(err)
				errLock.Unlock()
			}

			wg.Done()
		})
	}

	wg.Wait()
	return multiErr.FinalError()
}

func (d *downsamplerAndWriter) WriteBatch(
	ctx context.Context,
	iter DownsampleAndWriteIter,
	overrides WriteOptions,
) BatchError {
	d.logger.Info("WriteBatch called")

	// Add timing instrumentation
	totalStart := time.Now()
	var aggregatedDuration, unaggregatedDuration, waitDuration time.Duration

	var (
		wg       sync.WaitGroup
		multiErr xerrors.MultiError
		errLock  sync.Mutex
		addError = func(err error) {
			errLock.Lock()
			multiErr = multiErr.Add(err)
			errLock.Unlock()
		}
	)

	// Measure aggregated batch processing
	aggregatedStart := time.Now()
	if d.shouldDownsample(overrides) {
		d.logger.Info("WriteBatch: processing aggregated batch")
		if errs := d.writeAggregatedBatch(iter, overrides); !errs.Empty() {
			// Iterate and add through all the error to the multi error. It is
			// ok not to use the addError method here as we are running single
			// threaded at this point.
			for _, err := range errs.Errors() {
				multiErr = multiErr.Add(err)
			}
		}
	} else {
		d.logger.Info("WriteBatch: skipping aggregated batch (downsampling disabled)")
	}
	aggregatedDuration = time.Since(aggregatedStart)
	// Record aggregated phase latency (0 if no downsampling)
	d.metrics.aggregatedLatency.RecordDuration(aggregatedDuration)
	d.logger.Info("WriteBatch: aggregated phase completed", zap.Duration("duration", aggregatedDuration))

	// Reset the iter to write the unaggregated data.
	resetErr := iter.Reset()
	if resetErr != nil {
		addError(resetErr)
	}

	// Measure unaggregated batch processing
	unaggregatedStart := time.Now()
	if d.shouldWrite(overrides) && resetErr == nil {
		// Write unaggregated. Spin up all the background goroutines that make
		// network requests before we do the synchronous work of writing to the
		// downsampler.
		storagePolicies, ok := d.writeOverrideStoragePolicies(overrides)
		if !ok {
			storagePolicies = unaggregatedStoragePolicies
		}

		for iter.Next() {
			value := iter.Current()
			if value.Metadata.DropUnaggregated {
				d.logger.Info("WriteBatch: dropping unaggregated data for series")
				d.metrics.dropped.report(value.Attributes.Source)
				continue
			}

			d.metrics.written.report(value.Attributes.Source)
			if len(storagePolicies) > 1 {
				d.logger.Info("WriteBatch: multi-policy write", zap.Int("policies", len(storagePolicies)))
				d.metrics.multiPolicyWritten.reportInc(value.Attributes.Source, len(storagePolicies))
			}

			for idx, p := range storagePolicies {
				p := p // Capture for lambda.
				duplicateWrite := idx > 0
				wg.Add(1)
				d.workerPool.Go(func() {
					// Time individual store writes
					storeWriteStart := time.Now()
					d.logger.Info("WriteBatch: starting store write goroutine")
					// NB(r): Allocate the write query at the top
					// of the pooled worker instead of need to pass
					// the options down the stack which can cause
					// the stack to grow (and sometimes cause stack splits).
					writeQuery, err := storage.NewWriteQuery(storage.WriteQueryOptions{
						Tags:           value.Tags,
						Datapoints:     value.Datapoints,
						Unit:           value.Unit,
						Annotation:     value.Annotation,
						Attributes:     storageAttributesFromPolicy(p),
						DuplicateWrite: duplicateWrite,
					})
					if err == nil {
						err = d.store.Write(ctx, writeQuery)
					}
					storeWriteDuration := time.Since(storeWriteStart)

					// Record store write latency
					d.metrics.storeWriteLatency.RecordDuration(storeWriteDuration)
					d.logger.Info("WriteBatch: store write completed",
						zap.Duration("duration", storeWriteDuration),
						zap.Bool("error", err != nil))

					if err != nil {
						addError(err)
					}
					wg.Done()
				})
			}
		}
	} else {
		if resetErr != nil {
			d.logger.Info("WriteBatch: skipping unaggregated batch due to reset error", zap.Error(resetErr))
		} else if !d.shouldWrite(overrides) {
			d.logger.Info("WriteBatch: skipping unaggregated batch (writing disabled)")
		}
	}
	unaggregatedDuration = time.Since(unaggregatedStart)
	// Record unaggregated phase latency
	d.metrics.unaggregatedLatency.RecordDuration(unaggregatedDuration)
	d.logger.Info("WriteBatch: unaggregated phase completed", zap.Duration("duration", unaggregatedDuration))

	// Measure wait time
	waitStart := time.Now()
	wg.Wait()
	waitDuration = time.Since(waitStart)
	// Record wait latency
	d.metrics.waitLatency.RecordDuration(waitDuration)

	// Record total latency
	totalDuration := time.Since(totalStart)
	d.metrics.totalLatency.RecordDuration(totalDuration)

	d.logger.Info("WriteBatch completed",
		zap.Duration("total_duration", totalDuration),
		zap.Duration("aggregated_duration", aggregatedDuration),
		zap.Duration("unaggregated_duration", unaggregatedDuration),
		zap.Duration("wait_duration", waitDuration),
		zap.Int("errors", multiErr.NumErrors()))

	if multiErr.NumErrors() == 0 {
		return nil
	}

	return multiErr
}

func (d *downsamplerAndWriter) writeAggregatedBatch(
	iter DownsampleAndWriteIter,
	overrides WriteOptions,
) xerrors.MultiError {
	var multiErr xerrors.MultiError
	appender, err := d.downsampler.NewMetricsAppender()
	if err != nil {
		return multiErr.Add(err)
	}

	defer appender.Finalize()

	for iter.Next() {
		appender.NextMetric()

		value := iter.Current()
		if err := value.Tags.Validate(); err != nil {
			multiErr = multiErr.Add(err)
			continue
		}

		for _, tag := range value.Tags.Tags {
			appender.AddTag(tag.Name, tag.Value)
		}

		if value.Tags.Opts.IDSchemeType() == models.TypeGraphite {
			// NB(r): This is gross, but if this is a graphite metric then
			// we are going to set a special tag that means the downsampler
			// will write a graphite ID. This should really be plumbed
			// through the downsampler in general, but right now the aggregator
			// does not allow context to be attached to a metric so when it calls
			// back the context is lost currently.
			// TODO_FIX_GRAPHITE_TAGGING: Using this string constant to track
			// all places worth fixing this hack. There is at least one
			// other path where flows back to the coordinator from the aggregator
			// and this tag is interpreted, eventually need to handle more cleanly.
			appender.AddTag(downsample.MetricsOptionIDSchemeTagName,
				downsample.GraphiteIDSchemeTagValue)
		}

		opts := downsample.SampleAppenderOptions{
			SeriesAttributes: value.Attributes,
		}
		if downsampleMappingRuleOverrides, ok := d.downsampleOverrideRules(overrides); ok {
			opts = downsample.SampleAppenderOptions{
				Override: true,
				OverrideRules: downsample.SamplesAppenderOverrideRules{
					MappingRules: downsampleMappingRuleOverrides,
				},
			}
		}

		result, err := appender.SamplesAppender(opts)
		if err != nil {
			multiErr = multiErr.Add(err)
			continue
		}

		if result.IsDropPolicyApplied {
			iter.SetCurrentMetadata(ts.Metadata{DropUnaggregated: true})
		}

		for _, dp := range value.Datapoints {
			switch value.Attributes.M3Type {
			case ts.M3MetricTypeGauge:
				if result.ShouldDropTimestamp {
					err = result.SamplesAppender.AppendUntimedGaugeSample(dp.Timestamp, dp.Value, value.Annotation)
				} else {
					err = result.SamplesAppender.AppendGaugeSample(
						dp.Timestamp, dp.Value, value.Annotation,
					)
				}
			case ts.M3MetricTypeCounter:
				if result.ShouldDropTimestamp {
					err = result.SamplesAppender.AppendUntimedCounterSample(
						dp.Timestamp, int64(dp.Value), value.Annotation)
				} else {
					err = result.SamplesAppender.AppendCounterSample(
						dp.Timestamp, int64(dp.Value), value.Annotation,
					)
				}
			case ts.M3MetricTypeTimer:
				if result.ShouldDropTimestamp {
					err = result.SamplesAppender.AppendUntimedTimerSample(dp.Timestamp, dp.Value, value.Annotation)
				} else {
					err = result.SamplesAppender.AppendTimerSample(
						dp.Timestamp, dp.Value, value.Annotation,
					)
				}
			}
			if err != nil {
				// If we see an error break out so we can try processing the
				// next datapoint.
				multiErr = multiErr.Add(err)
			}
		}
	}

	return multiErr.Add(iter.Error())
}

func (d *downsamplerAndWriter) Downsampler() downsample.Downsampler {
	return d.downsampler
}

func (d *downsamplerAndWriter) Storage() storage.Storage {
	return d.store
}

func storageAttributesFromPolicy(
	p policy.StoragePolicy,
) storagemetadata.Attributes {
	attributes := storagemetadata.Attributes{
		MetricsType: storagemetadata.UnaggregatedMetricsType,
	}
	if p != unaggregatedStoragePolicy {
		attributes = storagemetadata.Attributes{
			// Assume all overridden storage policies are for aggregated namespaces.
			MetricsType: storagemetadata.AggregatedMetricsType,
			Resolution:  p.Resolution().Window,
			Retention:   p.Retention().Duration(),
		}
	}
	return attributes
}
