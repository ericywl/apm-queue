// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package kafka

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/metric/metricdata/metricdatatest"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	apmqueue "github.com/elastic/apm-queue/v2"
	"github.com/elastic/apm-queue/v2/queuecontext"
)

func TestProducerMetrics(t *testing.T) {
	test := func(ctx context.Context,
		t *testing.T,
		producer apmqueue.Producer,
		rdr sdkmetric.Reader,
		name string,
		want []metricdata.Metrics,
	) {
		topic := apmqueue.Topic(name)
		producer.Produce(ctx,
			apmqueue.Record{Topic: topic, Value: []byte("1")},
			apmqueue.Record{Topic: topic, Value: []byte("2")},
			apmqueue.Record{Topic: topic, Value: []byte("3")},
		)

		// Fixes https://github.com/elastic/apm-queue/issues/464
		<-time.After(time.Millisecond)

		// Close the producer so records are flushed.
		require.NoError(t, producer.Close())

		var rm metricdata.ResourceMetrics
		assert.NoError(t, rdr.Collect(context.Background(), &rm))

		metrics := filterMetrics(t, rm.ScopeMetrics)
		for _, m := range want {
			var actual metricdata.Metrics
			for _, mi := range metrics {
				if m.Name == mi.Name {
					actual = mi
					break
				}
			}
			assert.NotEmpty(t, actual)
			metricdatatest.AssertEqual(t, m, actual,
				metricdatatest.IgnoreTimestamp(),
				metricdatatest.IgnoreExemplars(),
				metricdatatest.IgnoreValue(),
			)
		}
	}
	t.Run("DeadlineExceeded", func(t *testing.T) {
		producer, rdr := setupTestProducer(t, nil)
		want := []metricdata.Metrics{
			{
				Name:        "producer.messages.count",
				Description: "The number of messages produced",
				Unit:        "1",
				Data: metricdata.Sum[int64]{
					Temporality: metricdata.CumulativeTemporality,
					IsMonotonic: true,
					DataPoints: []metricdata.DataPoint[int64]{
						{
							Value: 3,
							Attributes: attribute.NewSet(
								attribute.String("outcome", "failure"),
								attribute.String(errorReasonKey, "timeout"),
								attribute.String("namespace", "name_space"),
								attribute.String("topic", "name_space-default-topic"),
								semconv.MessagingSystem("kafka"),
								semconv.MessagingDestinationName("default-topic"),
								semconv.MessagingKafkaDestinationPartition(0),
							),
						},
					},
				},
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 0)
		defer cancel()
		test(ctx, t, producer, rdr, "default-topic", want)
	})
	t.Run("ContextCanceled", func(t *testing.T) {
		producer, rdr := setupTestProducer(t, nil)
		want := []metricdata.Metrics{
			{
				Name:        "producer.messages.count",
				Description: "The number of messages produced",
				Unit:        "1",
				Data: metricdata.Sum[int64]{
					Temporality: metricdata.CumulativeTemporality,
					IsMonotonic: true,
					DataPoints: []metricdata.DataPoint[int64]{
						{
							Value: 3, Attributes: attribute.NewSet(
								attribute.String("outcome", "failure"),
								attribute.String(errorReasonKey, "canceled"),
								attribute.String("namespace", "name_space"),
								attribute.String("topic", "name_space-default-topic"),
								semconv.MessagingSystem("kafka"),
								semconv.MessagingDestinationName("default-topic"),
								semconv.MessagingKafkaDestinationPartition(0),
							),
						},
					},
				},
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		test(ctx, t, producer, rdr, "default-topic", want)
	})
	t.Run("Unknown error reason", func(t *testing.T) {
		producer, rdr := setupTestProducer(t, nil)
		want := []metricdata.Metrics{{
			Name:        "producer.messages.count",
			Description: "The number of messages produced",
			Unit:        "1",
			Data: metricdata.Sum[int64]{
				Temporality: metricdata.CumulativeTemporality,
				IsMonotonic: true,
				DataPoints: []metricdata.DataPoint[int64]{
					{
						Value: 3,
						Attributes: attribute.NewSet(
							attribute.String("outcome", "failure"),
							attribute.String(errorReasonKey, "unknown"),
							attribute.String("namespace", "name_space"),
							attribute.String("topic", "name_space-default-topic"),
							semconv.MessagingSystem("kafka"),
							semconv.MessagingDestinationName("default-topic"),
							semconv.MessagingKafkaDestinationPartition(0),
						),
					},
				},
			},
		}}
		require.NoError(t, producer.Close())
		test(context.Background(), t, producer, rdr, "default-topic", want)
	})
	t.Run("unknown topic", func(t *testing.T) {
		producer, rdr := setupTestProducer(t, nil)
		want := []metricdata.Metrics{{
			Name:        "producer.messages.count",
			Description: "The number of messages produced",
			Unit:        "1",
			Data: metricdata.Sum[int64]{
				Temporality: metricdata.CumulativeTemporality,
				IsMonotonic: true,
				DataPoints: []metricdata.DataPoint[int64]{
					{
						Value: 3,
						Attributes: attribute.NewSet(
							attribute.String("outcome", "failure"),
							attribute.String(errorReasonKey,
								kerr.UnknownTopicOrPartition.Message,
							),
							attribute.String("namespace", "name_space"),
							attribute.String("topic", "name_space-unknown-topic"),
							semconv.MessagingSystem("kafka"),
							semconv.MessagingDestinationName("unknown-topic"),
							semconv.MessagingKafkaDestinationPartition(0),
						),
					},
				},
			},
		}}
		test(context.Background(), t, producer, rdr, "unknown-topic", want)
	})
	t.Run("Produced", func(t *testing.T) {
		producer, rdr := setupTestProducer(t, func(topic string) attribute.KeyValue {
			return attribute.String("test", "test")
		})
		want := []metricdata.Metrics{
			{
				Name:        "producer.messages.count",
				Description: "The number of messages produced",
				Unit:        "1",
				Data: metricdata.Sum[int64]{
					Temporality: metricdata.CumulativeTemporality,
					IsMonotonic: true,
					DataPoints: []metricdata.DataPoint[int64]{
						{
							Value: 3,
							Attributes: attribute.NewSet(
								attribute.String("outcome", "success"),
								attribute.String("namespace", "name_space"),
								attribute.String("topic", "name_space-default-topic"),
								semconv.MessagingSystem("kafka"),
								semconv.MessagingDestinationName("default-topic"),
								semconv.MessagingKafkaDestinationPartition(0),
								attribute.String("test", "test"),
								attribute.String("compression.codec", "none"),
							),
						},
					},
				},
			},
			{
				Name:        "producer.messages.wire.bytes",
				Description: "The number of bytes produced",
				Unit:        "By",
				Data: metricdata.Sum[int64]{
					Temporality: metricdata.CumulativeTemporality,
					IsMonotonic: true,
					DataPoints: []metricdata.DataPoint[int64]{
						{
							Value: 24,
							Attributes: attribute.NewSet(
								attribute.String("outcome", "success"),
								attribute.String("namespace", "name_space"),
								attribute.String("topic", "name_space-default-topic"),
								semconv.MessagingSystem("kafka"),
								semconv.MessagingDestinationName("default-topic"),
								semconv.MessagingKafkaDestinationPartition(0),
								attribute.String("test", "test"),
								attribute.String("compression.codec", "none"),
							),
						},
					},
				},
			},
			{
				Name:        "producer.messages.uncompressed.bytes",
				Description: "The number of uncompressed bytes produced",
				Unit:        "By",
				Data: metricdata.Sum[int64]{
					Temporality: metricdata.CumulativeTemporality,
					IsMonotonic: true,
					DataPoints: []metricdata.DataPoint[int64]{
						{
							Value: 24,
							Attributes: attribute.NewSet(
								attribute.String("outcome", "success"),
								attribute.String("namespace", "name_space"),
								attribute.String("topic", "name_space-default-topic"),
								semconv.MessagingSystem("kafka"),
								semconv.MessagingDestinationName("default-topic"),
								semconv.MessagingKafkaDestinationPartition(0),
								attribute.String("test", "test"),
								attribute.String("compression.codec", "none"),
							),
						},
					},
				},
			},
			{
				Name:        "messaging.kafka.write.latency",
				Description: "Time it took to write a batch including wait time before writing",
				Unit:        "s",
				Data: metricdata.Histogram[float64]{
					Temporality: metricdata.CumulativeTemporality,
					// do not check the value but only assert attributes
					// as it is tricky to control the latency
					DataPoints: []metricdata.HistogramDataPoint[float64]{{
						Attributes: attribute.NewSet(
							attribute.String("namespace", "name_space"),
							semconv.MessagingSystem("kafka"),
							attribute.String("operation", "ApiVersions"),
							attribute.String("outcome", "success"),
						)}, {
						Attributes: attribute.NewSet(
							attribute.String("namespace", "name_space"),
							semconv.MessagingSystem("kafka"),
							attribute.String("operation", "Metadata"),
							attribute.String("outcome", "success"),
						)}, {
						Attributes: attribute.NewSet(
							attribute.String("namespace", "name_space"),
							semconv.MessagingSystem("kafka"),
							attribute.String("operation", "InitProducerID"),
							attribute.String("outcome", "success"),
						)}, {
						Attributes: attribute.NewSet(
							attribute.String("namespace", "name_space"),
							semconv.MessagingSystem("kafka"),
							attribute.String("operation", "Produce"),
							attribute.String("outcome", "success"),
						)},
					}},
			},
		}
		test(context.Background(), t, producer, rdr, "default-topic", want)
	})
	t.Run("ProducedWithHeaders", func(t *testing.T) {
		producer, rdr := setupTestProducer(t, func(topic string) attribute.KeyValue {
			return attribute.String("some key", "some value")
		})
		want := []metricdata.Metrics{
			{
				Name:        "producer.messages.count",
				Description: "The number of messages produced",
				Unit:        "1",
				Data: metricdata.Sum[int64]{
					Temporality: metricdata.CumulativeTemporality,
					IsMonotonic: true,
					DataPoints: []metricdata.DataPoint[int64]{
						{
							Value: 3,
							Attributes: attribute.NewSet(
								attribute.String("outcome", "success"),
								attribute.String("namespace", "name_space"),
								attribute.String("topic", "name_space-default-topic"),
								semconv.MessagingSystem("kafka"),
								semconv.MessagingDestinationName("default-topic"),
								semconv.MessagingKafkaDestinationPartition(0),
								attribute.String("some key", "some value"),
								attribute.String("compression.codec", "snappy"),
							),
						},
					},
				},
			},
			{
				Name:        "producer.messages.wire.bytes",
				Description: "The number of bytes produced",
				Unit:        "By",
				Data: metricdata.Sum[int64]{
					Temporality: metricdata.CumulativeTemporality,
					IsMonotonic: true,
					DataPoints: []metricdata.DataPoint[int64]{
						{
							Value: 53,
							Attributes: attribute.NewSet(
								attribute.String("outcome", "success"),
								attribute.String("namespace", "name_space"),
								attribute.String("topic", "name_space-default-topic"),
								semconv.MessagingSystem("kafka"),
								semconv.MessagingDestinationName("default-topic"),
								semconv.MessagingKafkaDestinationPartition(0),
								attribute.String("some key", "some value"),
								attribute.String("compression.codec", "snappy"),
							),
						},
					},
				},
			},
			{
				Name:        "producer.messages.uncompressed.bytes",
				Description: "The number of uncompressed bytes produced",
				Unit:        "By",
				Data: metricdata.Sum[int64]{
					Temporality: metricdata.CumulativeTemporality,
					IsMonotonic: true,
					DataPoints: []metricdata.DataPoint[int64]{
						{
							Value: 114,
							Attributes: attribute.NewSet(
								attribute.String("outcome", "success"),
								attribute.String("namespace", "name_space"),
								attribute.String("topic", "name_space-default-topic"),
								semconv.MessagingSystem("kafka"),
								semconv.MessagingDestinationName("default-topic"),
								semconv.MessagingKafkaDestinationPartition(0),
								attribute.String("some key", "some value"),
								attribute.String("compression.codec", "snappy"),
							),
						},
					},
				},
			},
		}
		ctx := queuecontext.WithMetadata(context.Background(), map[string]string{
			"key":      "value",
			"some key": "some value",
		})
		test(ctx, t, producer, rdr, "default-topic", want)
	})
}

func TestConsumerMetrics(t *testing.T) {
	records := 10

	done := make(chan struct{})
	var processed atomic.Int64
	proc := apmqueue.ProcessorFunc(func(_ context.Context, r apmqueue.Record) error {
		processed.Add(1)
		if processed.Load() == int64(records) {
			close(done)
		}
		return nil
	})
	tc := setupTestConsumer(t, proc, func(topic string) attribute.KeyValue {
		return attribute.String("header", "included")
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	go func() { tc.consumer.Run(ctx) }() // Run Consumer.
	for i := 0; i < records; i++ {       // Produce records.
		produceRecord(ctx, t, tc.client, &kgo.Record{
			Topic: "name_space-" + t.Name(),
			Value: []byte(fmt.Sprint(i)),
			Headers: []kgo.RecordHeader{
				{Key: "header", Value: []byte("included")},
				{Key: "traceparent", Value: []byte("excluded")},
			},
		})
	}

	select {
	case <-ctx.Done():
		t.Error("Timed out while waiting for records to be consumed")
		return
	case <-done:
	}

	var rm metricdata.ResourceMetrics
	assert.NoError(t, tc.reader.Collect(context.Background(), &rm))

	wantMetrics := []metricdata.Metrics{
		{
			Name:        msgFetchedKey,
			Description: "The number of messages that were fetched from a kafka topic",
			Unit:        "1",
			Data: metricdata.Sum[int64]{
				Temporality: metricdata.CumulativeTemporality,
				IsMonotonic: true,
				DataPoints: []metricdata.DataPoint[int64]{
					{
						Value: int64(records),
						Attributes: attribute.NewSet(
							attribute.String("compression.codec", "none"),
							attribute.String("header", "included"),
							semconv.MessagingKafkaSourcePartition(0),
							semconv.MessagingSourceName(t.Name()),
							semconv.MessagingSystem("kafka"),
							attribute.String("namespace", "name_space"),
							attribute.String("topic", "name_space-TestConsumerMetrics"),
						),
					},
				},
			},
		},
		{
			Name:        "consumer.messages.delay",
			Description: "The delay between producing messages and reading them",
			Unit:        "s",
			Data: metricdata.Histogram[float64]{
				Temporality: metricdata.CumulativeTemporality,
				DataPoints: []metricdata.HistogramDataPoint[float64]{{
					Attributes: attribute.NewSet(
						attribute.String("header", "included"),
						semconv.MessagingKafkaSourcePartition(0),
						semconv.MessagingSourceName(t.Name()),
						semconv.MessagingSystem("kafka"),
						attribute.String("namespace", "name_space"),
						attribute.String("topic", "name_space-TestConsumerMetrics"),
					),

					Bounds: []float64{0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500, 10000},
					Count:  uint64(records),
				}},
			},
		},
		{
			Name:        "messaging.kafka.read.latency",
			Description: "Time it took to read a batch including wait time before reading",
			Unit:        "s",
			Data: metricdata.Histogram[float64]{
				Temporality: metricdata.CumulativeTemporality,
				DataPoints: []metricdata.HistogramDataPoint[float64]{{
					Attributes: attribute.NewSet(
						attribute.String("namespace", "name_space"),
						semconv.MessagingSystem("kafka"),
						attribute.String("outcome", "success"),
					),

					Bounds: []float64{0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500, 10000},
				}}},
		},
	}

	metrics := filterMetrics(t, rm.ScopeMetrics)
	for _, m := range wantMetrics {
		var metric metricdata.Metrics
		for _, mi := range metrics {
			if mi.Name == m.Name {
				metric = mi
				break
			}
		}

		assert.NotEmpty(t, metric)

		// Remove time-specific data for histograms
		if dp, ok := metric.Data.(metricdata.Histogram[float64]); ok {
			for k := range dp.DataPoints {
				dp.DataPoints[k].Min = m.Data.(metricdata.Histogram[float64]).DataPoints[k].Min
				dp.DataPoints[k].Max = m.Data.(metricdata.Histogram[float64]).DataPoints[k].Max
				dp.DataPoints[k].Sum = 0
				dp.DataPoints[k].BucketCounts = nil
			}
			metric.Data = dp
		}

		metricdatatest.AssertEqual(t,
			m,
			metric,
			metricdatatest.IgnoreTimestamp(),
			metricdatatest.IgnoreExemplars(),
			metricdatatest.IgnoreValue(),
		)
	}
}

func filterMetrics(t testing.TB, sm []metricdata.ScopeMetrics) []metricdata.Metrics {
	t.Helper()

	for _, m := range sm {
		if m.Scope.Name == instrumentName {
			return m.Metrics
		}
	}
	t.Fatal("unable to find metrics for", instrumentName)
	return []metricdata.Metrics{}
}

func setupTestProducer(t testing.TB, tafunc TopicAttributeFunc) (*Producer, sdkmetric.Reader) {
	t.Helper()

	rdr := sdkmetric.NewManualReader()
	brokers := newClusterAddrWithTopics(t, 1, "name_space-default-topic")
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr))
	t.Cleanup(func() {
		require.NoError(t, mp.Shutdown(context.Background()))
	})
	producer := newProducer(t, ProducerConfig{
		CommonConfig: CommonConfig{
			Brokers:            brokers,
			Logger:             zap.NewNop(),
			Namespace:          "name_space",
			TracerProvider:     noop.NewTracerProvider(),
			MeterProvider:      mp,
			TopicAttributeFunc: tafunc,
		},
		Sync: true,
	})
	return producer, rdr
}

type testMetricConsumer struct {
	consumer *Consumer
	client   *kgo.Client
	reader   sdkmetric.Reader
}

func setupTestConsumer(t testing.TB, p apmqueue.Processor, tafunc TopicAttributeFunc) (mc testMetricConsumer) {
	t.Helper()

	mc.reader = sdkmetric.NewManualReader()
	cfg := ConsumerConfig{
		Topics:    []apmqueue.Topic{apmqueue.Topic(t.Name())},
		GroupID:   t.Name(),
		Processor: p,
		CommonConfig: CommonConfig{
			Logger:         zap.NewNop(),
			Namespace:      "name_space",
			TracerProvider: noop.NewTracerProvider(),
			MeterProvider: sdkmetric.NewMeterProvider(
				sdkmetric.WithReader(mc.reader),
			),
			TopicAttributeFunc: tafunc,
		},
	}
	mc.client, cfg.Brokers = newClusterWithTopics(t, 1, "name_space-"+t.Name())
	mc.consumer = newConsumer(t, cfg)
	return
}
