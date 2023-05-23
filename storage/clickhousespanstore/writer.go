package clickhousespanstore

import (
	"context"
	"database/sql"
	"sync"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/jaegertracing/jaeger/model"
	"github.com/jaegertracing/jaeger/storage/spanstore"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/metadata"
)

type Encoding string

const (
	// EncodingJSON is used for spans encoded as JSON.
	EncodingJSON Encoding = "json"
	// EncodingProto is used for spans encoded as Protobuf.
	EncodingProto Encoding = "protobuf"
)

var (
	numWritesWithBatchSize = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jaeger_clickhouse_writes_with_batch_size_total",
		Help: "Number of clickhouse writes due to batch size criteria",
	})
	numWritesWithFlushInterval = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jaeger_clickhouse_writes_with_flush_interval_total",
		Help: "Number of clickhouse writes due to flush interval criteria",
	})
)

type SpanAndTenant struct {
	span   *model.Span
	tenant string
}

// SpanWriter for writing spans to ClickHouse
type SpanWriter struct {
	workerParams WorkerParams

	size   int64
	spans  chan SpanAndTenant
	finish chan bool
	done   sync.WaitGroup
}

var registerWriterMetrics sync.Once
var _ spanstore.Writer = (*SpanWriter)(nil)

// NewSpanWriter returns a SpanWriter for the database
func NewSpanWriter(
	logger hclog.Logger,
	db *sql.DB,
	indexTable,
	spansTable TableName,
	tenant string,
	encoding Encoding,
	delay time.Duration,
	size int64,
	maxSpanCount int,
) *SpanWriter {
	writer := &SpanWriter{
		workerParams: WorkerParams{
			logger:     logger,
			db:         db,
			indexTable: indexTable,
			spansTable: spansTable,
			tenant:     tenant,
			encoding:   encoding,
			delay:      delay,
		},
		size:   size,
		spans:  make(chan SpanAndTenant, size),
		finish: make(chan bool),
	}

	writer.registerMetrics()
	go writer.backgroundWriter(maxSpanCount)

	return writer
}

func (w *SpanWriter) registerMetrics() {
	registerWriterMetrics.Do(func() {
		prometheus.MustRegister(numWritesWithBatchSize)
		prometheus.MustRegister(numWritesWithFlushInterval)
	})
}

func (w *SpanWriter) backgroundWriter(maxSpanCount int) {
	pool := NewWorkerPool(&w.workerParams, maxSpanCount)
	go pool.Work()
	batch := make([]SpanAndTenant, 0, w.size)

	timer := time.After(w.workerParams.delay)
	last := time.Now()

	for {
		w.done.Add(1)

		flush := false
		finish := false

		select {
		case span := <-w.spans:
			batch = append(batch, span)
			flush = len(batch) == cap(batch)
			if flush {
				w.workerParams.logger.Debug("Flush due to batch size", "size", len(batch))
				numWritesWithBatchSize.Inc()
			}
		case <-timer:
			timer = time.After(w.workerParams.delay)
			flush = time.Since(last) > w.workerParams.delay && len(batch) > 0
			if flush {
				w.workerParams.logger.Debug("Flush due to timer")
				numWritesWithFlushInterval.Inc()
			}
		case <-w.finish:
			finish = true
			flush = len(batch) > 0
			w.workerParams.logger.Debug("Finish channel")
		}

		if flush {
			pool.WriteBatch(batch)

			batch = make([]SpanAndTenant, 0, w.size)
			last = time.Now()
		}

		if finish {
			pool.Close()
		}
		w.done.Done()

		if finish {
			break
		}
	}
}

// WriteSpan writes the encoded span
func (w *SpanWriter) WriteSpan(ctx context.Context, span *model.Span) error {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		tenants := md.Get("x-tenant")
		if len(tenants) == 0 {
			w.spans <- SpanAndTenant{span, ""}
		} else {
			w.spans <- SpanAndTenant{span, tenants[0]}
		}
	}

	return nil
}

// Close Implements io.Closer and closes the underlying storage
func (w *SpanWriter) Close() error {
	w.finish <- true
	w.done.Wait()
	return nil
}
