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
	"context"
	"io"
	"math/rand"
	"net/http"
	"testing"
	"time"

	"github.com/m3db/m3/src/query/models"
	"github.com/m3db/m3/src/query/storage"
	"github.com/m3db/m3/src/query/storage/m3/storagemetadata"
	"github.com/m3db/m3/src/query/storage/promremote/promremotetest"
	"github.com/m3db/m3/src/query/ts"
	"github.com/m3db/m3/src/x/tallytest"
	xtime "github.com/m3db/m3/src/x/time"

	"github.com/prometheus/prometheus/prompb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uber-go/tally"
	"go.uber.org/zap"
)

var (
	logger, _ = zap.NewDevelopment()
	scope     = tally.NewTestScope("test_scope", map[string]string{})

	tickDuration = time.Duration(100) * time.Millisecond
)

func TestWrite(t *testing.T) {
	fakeProm := promremotetest.NewServer(t)
	defer fakeProm.Close()
	promStorage, err := NewStorage(Options{
		endpoints:     []EndpointOptions{{name: "testEndpoint", address: fakeProm.WriteAddr(), tenantHeader: "TENANT"}},
		scope:         scope,
		logger:        logger,
		poolSize:      1,
		queueSize:     1,
		tenantDefault: "unknown",
		tickDuration:  ptrDuration(tickDuration),
	})
	require.NoError(t, err)
	defer closeWithCheck(t, promStorage)

	now := xtime.Now()
	wq, err := storage.NewWriteQuery(storage.WriteQueryOptions{
		Tags: models.Tags{
			Opts: models.NewTagOptions(),
			Tags: []models.Tag{{
				Name:  []byte("test_tag_name"),
				Value: []byte("test_tag_value"),
			}},
		},
		Datapoints: ts.Datapoints{{
			Timestamp: now,
			Value:     42,
		}},
		Unit: xtime.Millisecond,
	})
	require.NoError(t, err)
	err = promStorage.Write(context.TODO(), wq)
	require.NoError(t, err)
	promWrite := getWriteRequest(fakeProm)

	expectedLabel := prompb.Label{
		Name:  "test_tag_name",
		Value: "test_tag_value",
	}
	expectedSample := prompb.Sample{
		Value:     42,
		Timestamp: now.ToNormalizedTime(time.Millisecond),
	}
	require.Len(t, promWrite.Timeseries, 1)
	require.Len(t, promWrite.Timeseries[0].Labels, 1)
	require.Len(t, promWrite.Timeseries[0].Samples, 1)
	assert.Equal(t, expectedLabel, promWrite.Timeseries[0].Labels[0])
	assert.Equal(t, expectedSample, promWrite.Timeseries[0].Samples[0])

	tallytest.AssertCounterValue(
		t, 1, scope.Snapshot(), "test_scope.prom_remote_storage.write.total",
		map[string]string{"endpoint_name": "testEndpoint", "code": "200"},
	)
}

func TestWriteBasedOnRetention(t *testing.T) {
	promShortRetention := promremotetest.NewServer(t)
	defer promShortRetention.Close()
	promMediumRetention := promremotetest.NewServer(t)
	defer promMediumRetention.Close()
	promLongRetention := promremotetest.NewServer(t)
	defer promLongRetention.Close()
	promLongRetention2 := promremotetest.NewServer(t)
	defer promLongRetention2.Close()
	reset := func() {
		promShortRetention.Reset()
		promMediumRetention.Reset()
		promLongRetention.Reset()
		promLongRetention2.Reset()
	}

	mediumRetentionAttr := storagemetadata.Attributes{
		MetricsType: storagemetadata.AggregatedMetricsType,
		Retention:   720 * time.Hour,
		Resolution:  5 * time.Minute,
	}
	shortRetentionAttr := storagemetadata.Attributes{
		MetricsType: storagemetadata.AggregatedMetricsType,
		Retention:   120 * time.Hour,
		Resolution:  15 * time.Second,
	}
	longRetentionAttr := storagemetadata.Attributes{
		Resolution: 10 * time.Minute,
		Retention:  8760 * time.Hour,
	}
	getPromStorage := func() storage.Storage {
		promStorage, err := NewStorage(Options{
			endpoints: []EndpointOptions{
				// always write to the first endpoint
				{
					address:      promShortRetention.WriteAddr(),
					attributes:   shortRetentionAttr,
					tenantHeader: "TENANT",
				},
				{
					address:      promMediumRetention.WriteAddr(),
					attributes:   mediumRetentionAttr,
					tenantHeader: "TENANT",
				},
				{
					address:      promLongRetention.WriteAddr(),
					attributes:   longRetentionAttr,
					tenantHeader: "TENANT",
				},
				{
					address:      promLongRetention2.WriteAddr(),
					attributes:   longRetentionAttr,
					tenantHeader: "TENANT",
				},
			},
			poolSize:      1,
			queueSize:     9,
			scope:         scope,
			logger:        logger,
			tenantDefault: "unknown",
			tickDuration:  ptrDuration(tickDuration),
		})
		require.NoError(t, err)
		return promStorage
	}
	t.Run("send short retention write", func(t *testing.T) {
		reset()
		promStorage := getPromStorage()
		err := writeTestMetric(t, promStorage, shortRetentionAttr)
		require.NoError(t, err)

		// Close() ensures writes get flushed
		require.NoError(t, promStorage.Close())

		assert.NotNil(t, getWriteRequest(promShortRetention))
		assert.Nil(t, getWriteRequest(promMediumRetention))
		assert.Nil(t, getWriteRequest(promLongRetention))
	})

	t.Run("send medium retention write", func(t *testing.T) {
		reset()
		promStorage := getPromStorage()
		err := writeTestMetric(t, promStorage, mediumRetentionAttr)
		require.NoError(t, err)

		// Close() ensures writes get flushed
		require.NoError(t, promStorage.Close())

		assert.NotNil(t, getWriteRequest(promShortRetention))
	})

	t.Run("send write to multiple instances configured with same retention", func(t *testing.T) {
		reset()
		promStorage := getPromStorage()
		err := writeTestMetric(t, promStorage, longRetentionAttr)
		require.NoError(t, err)

		// Close() ensures writes get flushed
		require.NoError(t, promStorage.Close())

		assert.NotNil(t, getWriteRequest(promShortRetention))
		assert.Nil(t, getWriteRequest(promMediumRetention))
		assert.Nil(t, getWriteRequest(promLongRetention))
		assert.Nil(t, getWriteRequest(promLongRetention2))
	})

	t.Run("send unconfigured retention write", func(t *testing.T) {
		reset()
		promStorage := getPromStorage()
		writeTestMetric(t, promStorage, storagemetadata.Attributes{
			Resolution: mediumRetentionAttr.Resolution + 1,
			Retention:  mediumRetentionAttr.Retention,
		})
		writeTestMetric(t, promStorage, storagemetadata.Attributes{
			Resolution: mediumRetentionAttr.Resolution,
			Retention:  mediumRetentionAttr.Retention + 1,
		})

		// Close() ensures writes get flushed
		require.NoError(t, promStorage.Close())

		// All writes get dropped because of "no pre-defined tenant found"
		assert.Nil(t, getWriteRequest(promShortRetention))
		assert.Nil(t, getWriteRequest(promMediumRetention))
		assert.Nil(t, getWriteRequest(promLongRetention))
		const droppedWrites = "test_scope.prom_remote_storage.dropped_writes"
		tallytest.AssertCounterValue(t, 2, scope.Snapshot(), droppedWrites, map[string]string{})
	})

	t.Run("error should not prevent sending to other instances", func(t *testing.T) {
		reset()
		promStorage := getPromStorage()
		promLongRetention.SetError("test err", http.StatusInternalServerError)
		writeTestMetric(t, promStorage, longRetentionAttr)

		// Close() ensures writes get flushed
		require.NoError(t, promStorage.Close())

		assert.NotNil(t, getWriteRequest(promShortRetention))
	})
}

func TestErrorHandling(t *testing.T) {
	svr := promremotetest.NewServer(t)
	defer svr.Close()

	attr := storagemetadata.Attributes{
		MetricsType: storagemetadata.AggregatedMetricsType,
		Retention:   720 * time.Hour,
		Resolution:  5 * time.Minute,
	}
	getPromStorage := func(scope tally.Scope) storage.Storage {
		promStorage, err := NewStorage(Options{
			endpoints:     []EndpointOptions{{name: "testEndpoint", address: svr.WriteAddr(), attributes: attr, tenantHeader: "TENANT"}},
			poolSize:      1,
			queueSize:     1,
			scope:         scope,
			logger:        logger,
			tenantDefault: "unknown",
			tickDuration:  ptrDuration(tickDuration),
		})
		require.NoError(t, err)
		return promStorage
	}

	t.Run("wrap non 5xx errors as invalid params error", func(t *testing.T) {
		svr.Reset()
		svr.SetError("test err", http.StatusForbidden)

		scope := tally.NewTestScope("5xx_test_scope", map[string]string{})
		promStorage := getPromStorage(scope)
		err := writeTestMetric(t, promStorage, attr)
		require.NoError(t, err)

		// Close() ensures writes get flushed
		require.NoError(t, promStorage.Close())

		tallytest.AssertCounterValue(
			t, 1, scope.Snapshot(), "5xx_test_scope.prom_remote_storage.write.total",
			map[string]string{"endpoint_name": "testEndpoint", "code": "403"},
		)
		tallytest.AssertCounterValue(
			t, 1, scope.Snapshot(), "5xx_test_scope.prom_remote_storage.batch_write_err",
			map[string]string{},
		)
	})

	t.Run("409 is not an error", func(t *testing.T) {
		svr.Reset()
		svr.SetError("test err", http.StatusConflict)

		scope := tally.NewTestScope("409_test_scope", map[string]string{})
		promStorage := getPromStorage(scope)
		err := writeTestMetric(t, promStorage, attr)
		require.NoError(t, err)

		// Close() ensures writes get flushed
		require.NoError(t, promStorage.Close())

		tallytest.AssertCounterValue(
			t, 1, scope.Snapshot(), "409_test_scope.prom_remote_storage.write.total",
			map[string]string{"endpoint_name": "testEndpoint", "code": "409"},
		)
		tallytest.AssertCounterValue(
			t, 0, scope.Snapshot(), "409_test_scope.prom_remote_storage.batch_write_err",
			map[string]string{},
		)
	})
}

func closeWithCheck(t *testing.T, c io.Closer) {
	require.NoError(t, c.Close())
}

func writeTestMetric(t *testing.T, s storage.Storage, attr storagemetadata.Attributes) error {
	//nolint: gosec
	datapoint := ts.Datapoint{Value: rand.Float64(), Timestamp: xtime.Now()}
	wq, err := storage.NewWriteQuery(storage.WriteQueryOptions{
		Tags: models.Tags{
			Opts: models.NewTagOptions(),
			Tags: []models.Tag{{
				Name:  []byte("test_tag_name"),
				Value: []byte("test_tag_value"),
			}},
		},
		Datapoints: ts.Datapoints{datapoint},
		Unit:       xtime.Millisecond,
		Attributes: attr,
	})
	require.NoError(t, err)
	return s.Write(context.TODO(), wq)
}

func getWriteRequest(promServer *promremotetest.TestPromServer) *prompb.WriteRequest {
	wq := promServer.GetLastWriteRequest()
	for retries := 0; wq == nil && retries < 10; retries++ {
		time.Sleep(tickDuration)
		wq = promServer.GetLastWriteRequest()
	}
	return wq
}
