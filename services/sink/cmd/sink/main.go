// Command sink is the Week-2 Go sink service. It replaces the off-the-shelf
// MongoDB Kafka Connector (which lost 1 row in the chaos 01 run documented
// in docs/chaos-findings.md) with a consume loop that structurally enforces
// commit-after-side-effect + LSN-gated idempotent upserts.
//
// Config via env (all have sensible defaults for the docker-compose wiring):
//
//	KAFKA_BROKERS        comma-separated, default "kafka:29092"
//	KAFKA_GROUP_ID       default "zdt-sink"
//	KAFKA_TOPIC_REGEX    default "^cdc\\..*"
//	MONGO_URI            default "mongodb://mongo:27017/?replicaSet=rs0"
//	MONGO_DB             default "migration"
//	SCHEMA_VERSION       default 1
//	METRICS_ADDR         default ":8080"
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"zdt/sink/internal/consumer"
	"zdt/sink/internal/kafka"
	"zdt/sink/internal/metrics"
	"zdt/sink/internal/writer"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func main() {
	brokers := strings.Split(env("KAFKA_BROKERS", "kafka:29092"), ",")
	groupID := env("KAFKA_GROUP_ID", "zdt-sink")
	topicRegex := env("KAFKA_TOPIC_REGEX", `^cdc\..*`)
	mongoURI := env("MONGO_URI", "mongodb://mongo:27017/?replicaSet=rs0")
	mongoDB := env("MONGO_DB", "migration")
	metricsAddr := env("METRICS_ADDR", ":8080")
	schemaVer := envInt("SCHEMA_VERSION", 1)

	log.Printf("sink starting: brokers=%v topic=%s group=%s mongo=%s/%s metrics=%s schemaVer=%d",
		brokers, topicRegex, groupID, mongoURI, mongoDB, metricsAddr, schemaVer)

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Mongo
	mClient, err := mongo.Connect(options.Client().ApplyURI(mongoURI))
	if err != nil {
		log.Fatalf("mongo connect: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mClient.Disconnect(ctx)
	}()
	mongoWriter := writer.NewMongoWriter(mClient, mongoDB, schemaVer)

	// Metrics + health server
	m := metrics.New()
	srv := &http.Server{
		Addr:              metricsAddr,
		ReadHeaderTimeout: 5 * time.Second,
		Handler:           buildHTTPMux(m),
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("metrics server: %v", err)
		}
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	// Kafka
	kc, err := kafka.New(brokers, groupID, topicRegex)
	if err != nil {
		log.Fatalf("kafka new: %v", err)
	}
	defer kc.Close()

	// Instrumented writer wraps MongoWriter to record metrics per event.
	instrumented := newInstrumentedWriter(mongoWriter, m)
	loop := &consumer.Loop{Cons: kc, W: instrumented, SchemaVer: schemaVer}

	runLoop(rootCtx, loop)
	log.Printf("sink stopped")
}

func runLoop(ctx context.Context, loop *consumer.Loop) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := loop.RunOnce(ctx); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			log.Printf("loop error, backoff 1s: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

func buildHTTPMux(m *metrics.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.HTTPHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

// instrumentedWriter decorates a Writer with Prometheus counters. Kept in
// main.go (not internal/metrics) because it is composition of the two —
// neither package owns both.
type instrumentedWriter struct {
	inner consumer.Writer
	m     *metrics.Metrics
}

func newInstrumentedWriter(inner consumer.Writer, m *metrics.Metrics) *instrumentedWriter {
	return &instrumentedWriter{inner: inner, m: m}
}

func (i *instrumentedWriter) ApplyBatch(ctx context.Context, evs []writer.CDCEvent) error {
	if err := i.inner.ApplyBatch(ctx, evs); err != nil {
		i.m.WriteErrors.WithLabelValues("mongo", classify(err)).Inc()
		return err
	}
	for _, ev := range evs {
		i.m.EventsProcessed.WithLabelValues("sink", ev.Table, string(ev.Op)).Inc()
	}
	return nil
}

func classify(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "context"):
		return "context"
	case strings.Contains(msg, "connection"):
		return "connection"
	default:
		return "other"
	}
}

// --- env helpers ---

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("env %s: not an int %q, using default %d", key, v, def)
	}
	return def
}
