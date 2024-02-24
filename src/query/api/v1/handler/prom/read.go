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

// Package prom provides custom handlers that support the prometheus
// query endpoints.
package prom

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/m3db/m3/src/query/api/v1/handler/prometheus/handleroptions"
	"github.com/m3db/m3/src/query/api/v1/handler/prometheus/native"
	"github.com/m3db/m3/src/query/api/v1/options"
	"github.com/m3db/m3/src/query/block"
	queryerrors "github.com/m3db/m3/src/query/errors"
	"github.com/m3db/m3/src/query/models"
	"github.com/m3db/m3/src/query/storage"
	"github.com/m3db/m3/src/query/storage/prometheus"
	xerrors "github.com/m3db/m3/src/x/errors"
	xhttp "github.com/m3db/m3/src/x/net/http"

	xsync "github.com/m3db/m3/src/x/sync"
	errs "github.com/pkg/errors"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	promstorage "github.com/prometheus/prometheus/storage"
	"github.com/uber-go/tally"
	"go.uber.org/zap"
)

const (
	// Query series Warning limit 100k
	querySeriesWarn = 1e5

	// Query max size for metric
	truncatedQueryLimit = 1024
)

// NewQueryFn creates a new promql Query.
type NewQueryFn func(params models.RequestParams) (promql.Query, error)

var (
	newRangeQueryFn = func(
		engineFn options.PromQLEngineFn,
		queryable promstorage.Queryable,
	) NewQueryFn {
		return func(params models.RequestParams) (promql.Query, error) {
			engine, err := engineFn(params.LookbackDuration)
			if err != nil {
				return nil, err
			}
			return engine.NewRangeQuery(
				queryable,
				params.Query,
				params.Start.ToTime(),
				params.End.ToTime(),
				params.Step)
		}
	}

	newInstantQueryFn = func(
		engineFn options.PromQLEngineFn,
		queryable promstorage.Queryable,
	) NewQueryFn {
		return func(params models.RequestParams) (promql.Query, error) {
			engine, err := engineFn(params.LookbackDuration)
			if err != nil {
				return nil, err
			}
			return engine.NewInstantQuery(
				queryable,
				params.Query,
				params.Now)
		}
	}
)

type readHandler struct {
	hOpts               options.HandlerOptions
	scope               tally.Scope
	logger              *zap.Logger
	opts                opts
	returnedDataMetrics native.PromReadReturnedDataMetrics
	qs                  *queryShadowing
}

func newReadHandler(
	hOpts options.HandlerOptions,
	options opts,
) (http.Handler, error) {
	scope := hOpts.InstrumentOpts().MetricsScope().Tagged(
		map[string]string{"handler": "prometheus-read"},
	)
	var qs *queryShadowing = nil
	if hOpts.ShadowQueryURL() != "" {
		qs = newQueryShadowing(hOpts.ShadowQueryURL(), hOpts.QueryShadowingWorkers(), scope)
	}
	handler := &readHandler{
		hOpts:               hOpts,
		opts:                options,
		scope:               scope,
		logger:              hOpts.InstrumentOpts().Logger(),
		returnedDataMetrics: native.NewPromReadReturnedDataMetrics(scope),
		qs: 			     qs,
	}
	if handler.qs != nil {
		handler.logger.Info("Query shadowing is enabled",
		    zap.String("shadowQueryURL", handler.qs.shadowQueryURL),
			zap.Int("QueryShadowingWorkers", hOpts.QueryShadowingWorkers()),
		)
	}
	return handler, nil
}

type queryShadowing struct {
	// This URL doesn't includes the path, "api/v1/query_range" or "api/v1/query".
	// It shouldn't end with a slash('/').
	shadowQueryURL string
	workerPool     xsync.WorkerPool
	client         *http.Client
	failedQueryCounter tally.Counter
	respondedQueryCounter tally.Counter
	responded2xxQueryCounter tally.Counter
	skippedQueryCounter tally.Counter
}

func getHttpClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 10
	t.MaxConnsPerHost = 10
	t.MaxIdleConnsPerHost = 10
	return &http.Client{
		Timeout:   8 * 60 * time.Second,
		Transport: t,
	}
}

func newQueryShadowing(shadowQueryURL string, numWorkers int, scope tally.Scope) *queryShadowing {
	workerPool := xsync.NewWorkerPool(numWorkers)
	workerPool.Init()
	return &queryShadowing{
		shadowQueryURL: shadowQueryURL,
		workerPool:     workerPool,
		client:         getHttpClient(),
		failedQueryCounter: scope.Counter("failed_shadow_query"),
		respondedQueryCounter: scope.Counter("responded_shadow_query"),
		responded2xxQueryCounter: scope.Counter("2xx_shadow_query"),
		skippedQueryCounter: scope.Counter("skipped_shadow_query"),
	}
}

func (h* readHandler) sendShadowQuery(r *http.Request) {
	if (h.qs == nil) {
		return
	}
	// Forward the requests to h.qs.shadowQueryURL
	shadowURL := h.qs.shadowQueryURL
	if strings.HasPrefix(r.URL.Path, "/") {
		shadowURL += r.URL.Path
	} else {
		shadowURL += "/" + r.URL.Path
	}
	if r.URL.RawQuery != "" {
		shadowURL += "?" + r.URL.RawQuery
	}
	var requestBody io.Reader = r.Body
	if r.Method == "POST" {
		// If it's a POST request, r.Body has already been read and parsed into r.PostForm.
		// r.Body can't be parsed twice.
		requestBody = strings.NewReader(r.PostForm.Encode())
	}
	shadowReq, err := http.NewRequest(r.Method, shadowURL, requestBody)
	if err != nil {
		h.logger.Error("Failed to create a shadow http request", zap.Error(err), zap.String("shadowURL", shadowURL))
		h.qs.skippedQueryCounter.Inc(1)
		return
	}
	shadowReq.Header = r.Header
	doSend := func() {
		// All goroutines sharing the same http client is fine and actually recommended. Under the hood, the http client
		// use a connection pool to reuse connections.
		resp, err := h.qs.client.Do(shadowReq)
		if err != nil {
			h.logger.Error("The shadow http request failed", zap.Error(err), zap.String("shadowURL", shadowURL))
			h.qs.failedQueryCounter.Inc(1)
			return
		}
		// The response body is thrown away because we only care about request success/failure instead of correctness.
		// NB: we need to read all the response body and close the body to reuse the connection.
		// The following comment is from net/http source code
		// If the returned error is nil, the Response will contain a non-nil 
		// Body which the user is expected to close. If the Body is not both 
		// read to EOF and closed, the Client's underlying RoundTripper 
		// (typically Transport) may not be able to re-use a persistent TCP 
		// connection to the server for a subsequent "keep-alive" request.
		_, err = io.ReadAll(resp.Body)
		defer resp.Body.Close()
		if err != nil {
			h.logger.Error("The shadow http response failed to read", zap.Error(err), zap.String("shadowURL", shadowURL))
			h.qs.failedQueryCounter.Inc(1)
			return
		}
		h.qs.respondedQueryCounter.Inc(1)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			h.qs.responded2xxQueryCounter.Inc(1)
			h.logger.Debug("Shadow query got a 2xx response",
				zap.String("shadowURL", shadowURL),
				zap.Int("statusCode", resp.StatusCode),
				zap.Int64("responseContentLength", resp.ContentLength),
			)
		} else {
			h.logger.Error("Shadow query got a non-2xx response",
				zap.String("shadowURL", shadowURL),
				zap.Int("statusCode", resp.StatusCode),
				zap.Int64("responseContentLength", resp.ContentLength),
			)
		}
	}
	if !h.qs.workerPool.GoWithTimeout(doSend, time.Second * 3) {
		h.logger.Error("Failed to send shadow query because worker pool can't catch up with the pending requests",
			zap.Int("workerPoolCapacity", h.qs.workerPool.Size()),
		)
		h.qs.skippedQueryCounter.Inc(1)
	}
}

func (h *readHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ctx, request, err := native.ParseRequest(ctx, r, h.opts.instant, h.hOpts)
	if err != nil {
		xhttp.WriteError(w, err)
		return
	}

	h.sendShadowQuery(r)

	params := request.Params
	fetchOptions := request.FetchOpts

	// NB (@shreyas): We put the FetchOptions in context so it can be
	// retrieved in the queryable object as there is no other way to pass
	// that through.
	//
	// We also put a function into the context that allows callers to safely
	// pass back result metadata concurrently so that they can be combined
	// for later reporting.
	var resultMetadataMutex sync.Mutex
	resultMetadata := block.NewResultMetadata()
	resultMetadataReceiveFn := func(m block.ResultMetadata) {
		resultMetadataMutex.Lock()
		defer resultMetadataMutex.Unlock()
		resultMetadata = resultMetadata.CombineMetadata(m)
	}
	ctx = context.WithValue(ctx, prometheus.FetchOptionsContextKey, fetchOptions)
	ctx = context.WithValue(ctx, prometheus.BlockResultMetadataFnKey, resultMetadataReceiveFn)

	qry, err := h.opts.newQueryFn(params)
	if err != nil {
		h.logger.Error("error creating query",
			zap.Error(err), zap.String("query", params.Query),
			zap.Bool("instant", h.opts.instant))
		xhttp.WriteError(w, xerrors.NewInvalidParamsError(err))
		return
	}
	defer qry.Close()

	res := qry.Exec(ctx)
	if res.Err != nil {
		h.logger.Error("error executing query",
			zap.Error(res.Err), zap.String("query", params.Query),
			zap.Bool("instant", h.opts.instant))
		var sErr *prometheus.StorageErr
		if errors.As(res.Err, &sErr) {
			// If the error happened in the m3 storage layer, propagate the causing error as is.
			err := sErr.Unwrap()
			if queryerrors.IsTimeout(err) {
				xhttp.WriteError(w, queryerrors.NewErrQueryTimeout(err))
			} else {
				xhttp.WriteError(w, err)
			}
		} else {
			promErr := errs.Cause(res.Err)
			switch promErr.(type) { //nolint:errorlint
			case promql.ErrQueryTimeout:
				promErr = queryerrors.NewErrQueryTimeout(promErr)
			case promql.ErrQueryCanceled:
			default:
				// Assume any prometheus library error is a 4xx, since there are no remote calls.
				promErr = xerrors.NewInvalidParamsError(res.Err)
			}
			xhttp.WriteError(w, promErr)
		}
		return
	}

	for _, warn := range resultMetadata.Warnings {
		res.Warnings = append(res.Warnings, errors.New(warn.Message))
	}

	query := params.Query
	err = ApplyRangeWarnings(query, &resultMetadata)
	if err != nil {
		h.logger.Warn("error applying range warnings",
			zap.Error(err), zap.String("query", query),
			zap.Bool("instant", h.opts.instant))
	}

	err = handleroptions.AddDBResultResponseHeaders(w, resultMetadata, fetchOptions)
	if err != nil {
		h.logger.Error("error writing database limit headers", zap.Error(err))
		xhttp.WriteError(w, err)
		return
	}

	returnedDataLimited := h.limitReturnedData(query, res, fetchOptions)
	h.returnedDataMetrics.FetchM3Series.RecordValue(float64(resultMetadata.FetchedSeriesCount))
	h.returnedDataMetrics.FetchDatapoints.RecordValue(float64(returnedDataLimited.Datapoints))
	h.returnedDataMetrics.FetchSeries.RecordValue(float64(returnedDataLimited.Series))

	// if query return data more than warning limit, logging an as warning
	if resultMetadata.FetchedSeriesCount > querySeriesWarn {
		metricName := h.extractMetricName(query)
		h.logger.Warn("The time series query return more than query limit", zap.Int("limit threshold", querySeriesWarn),
			zap.Int("time series", resultMetadata.FetchedSeriesCount), zap.String("metric", metricName), zap.String("query", query))

		truncatedQuery := h.truncateQuery(query)
		gauge, exists := h.returnedDataMetrics.OverLimitFetchM3Series[metricName]
		if !exists {
			gauge = h.returnedDataMetrics.Scope.Tagged(
				map[string]string{"query": truncatedQuery, "metric": metricName},
			).Gauge("fetch.over_limit_m3_series")
			h.returnedDataMetrics.OverLimitFetchM3Series[truncatedQuery] = gauge
		}
		gauge.Update(float64(resultMetadata.FetchedSeriesCount))
	}

	limited := &handleroptions.ReturnedDataLimited{
		Limited:     returnedDataLimited.Limited,
		Series:      returnedDataLimited.Series,
		TotalSeries: returnedDataLimited.TotalSeries,
		Datapoints:  returnedDataLimited.Datapoints,
	}
	err = handleroptions.AddReturnedLimitResponseHeaders(w, limited, nil)
	if err != nil {
		h.logger.Error("error writing response headers",
			zap.Error(err), zap.String("query", query),
			zap.Bool("instant", h.opts.instant))
		xhttp.WriteError(w, err)
		return
	}

	if err := Respond(w, &QueryData{
		Result:     res.Value,
		ResultType: res.Value.Type(),
	}, res.Warnings); err != nil {
		h.logger.Error("error writing prom response",
			zap.Error(err),
			zap.String("query", params.Query),
			zap.Bool("instant", h.opts.instant))
	}
}

// NB: this is a naive but lightweight method to extra a metric name from a PromQL query.
// It returns an empty string if it fails to extract a metric name.
// We don't want to parse the PromQL here because the extraction is not super important.
func (h *readHandler) extractMetricName(query string) string {
	// Some example queries:
	//  sum by (namespace) (increase(kube_pod_container_status_restarts_total{namespace!~"test-.+",pod=~"data-plane-router.*"}[10m] ...
	//  histogram_quantile(0.5, sum by (shardName, kubernetes_namespace, project, client_name, jetty_request_type, status, hmr_role, le) (rate(rpc_client_request_duration_seconds_bucket[10m])))
	// We assume the token before '{' or '[' is a metric name.
	endPos := strings.IndexAny(query, "{[")
	
	isMetricNameByte := func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
		  (b >= '0' && b <= '9') || strings.IndexByte("._:-", b) >= 0
	}

	// This is to skip any trailing whitespace.
	for endPos > 0 && !isMetricNameByte(query[endPos - 1]) {
		endPos--
	}
	if endPos <= 0 {
		return ""
	}
	// Invariant: query[startPos] is a metric byte.
	startPos := endPos - 1
	for startPos > 0 && isMetricNameByte(query[startPos - 1]) {
		startPos--
	}
	return query[startPos:endPos]
}

func (h *readHandler) truncateQuery(query string) string {
	if len(query) <= truncatedQueryLimit {
		return query
	}
	return query[:truncatedQueryLimit] + "..."
}

func (h *readHandler) limitReturnedData(query string,
	res *promql.Result,
	fetchOpts *storage.FetchOptions,
) native.ReturnedDataLimited {
	var (
		seriesLimit     = fetchOpts.ReturnedSeriesLimit
		datapointsLimit = fetchOpts.ReturnedDatapointsLimit

		limited     = false
		series      int
		datapoints  int
		seriesTotal int
	)
	switch res.Value.Type() {
	case parser.ValueTypeVector:
		v, err := res.Vector()
		if err != nil {
			h.logger.Error("error parsing vector for returned data limits",
				zap.Error(err), zap.String("query", query),
				zap.Bool("instant", h.opts.instant))
			break
		}

		// Determine maxSeries based on either series or datapoints limit. Vector has one datapoint per
		// series and so the datapoint limit behaves the same way as the series one.
		switch {
		case seriesLimit > 0 && datapointsLimit == 0:
			series = seriesLimit
		case seriesLimit == 0 && datapointsLimit > 0:
			series = datapointsLimit
		case seriesLimit == 0 && datapointsLimit == 0:
			// Set max to the actual size if no limits.
			series = len(v)
		default:
			// Take the min of the two limits if both present.
			series = seriesLimit
			if seriesLimit > datapointsLimit {
				series = datapointsLimit
			}
		}

		seriesTotal = len(v)
		limited = series < seriesTotal

		if limited {
			limitedSeries := v[:series]
			res.Value = limitedSeries
			datapoints = len(limitedSeries)
		} else {
			series = seriesTotal
			datapoints = seriesTotal
		}
	case parser.ValueTypeMatrix:
		m, err := res.Matrix()
		if err != nil {
			h.logger.Error("error parsing vector for returned data limits",
				zap.Error(err), zap.String("query", query),
				zap.Bool("instant", h.opts.instant))
			break
		}

		for _, d := range m {
			datapointCount := len(d.Points)
			if fetchOpts.ReturnedSeriesLimit > 0 && series+1 > fetchOpts.ReturnedSeriesLimit {
				limited = true
				break
			}
			if fetchOpts.ReturnedDatapointsLimit > 0 && datapoints+datapointCount > fetchOpts.ReturnedDatapointsLimit {
				limited = true
				break
			}
			series++
			datapoints += datapointCount
		}
		seriesTotal = len(m)

		if series < seriesTotal {
			res.Value = m[:series]
		}
	default:
	}

	return native.ReturnedDataLimited{
		Limited:     limited,
		Series:      series,
		Datapoints:  datapoints,
		TotalSeries: seriesTotal,
	}
}
