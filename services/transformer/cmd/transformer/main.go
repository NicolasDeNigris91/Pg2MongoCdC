// Command transformer reads CDC events from `cdc.*`, rewrites field names
// per schema/transforms/<table>.yml, and publishes to `transformed.*`.
// Offsets commit only after produce is ack'd; redelivery on failure is a
// no-op downstream because the sink's LSN gate absorbs duplicates.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"transformer/internal/mapper"

	"github.com/twmb/franz-go/pkg/kgo"
)

func main() {
	brokers := strings.Split(env("KAFKA_BROKERS", "kafka:29092"), ",")
	groupID := env("KAFKA_GROUP_ID", "zdt-transformer")
	topicRegex := env("KAFKA_TOPIC_REGEX", `^cdc\..*`)
	rulesDir := env("RULES_DIR", "/etc/transformer/rules")
	metricsAddr := env("METRICS_ADDR", ":8080")

	log.Printf("transformer starting: brokers=%v source-topic=%s rules=%s", brokers, topicRegex, rulesDir)

	m, err := mapper.Load(rulesDir)
	if err != nil {
		log.Fatalf("mapper: %v", err)
	}
	log.Printf("loaded %d rule(s)", len(m.Rules()))

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(groupID),
		kgo.ConsumeRegex(),
		kgo.ConsumeTopics(topicRegex),
		kgo.DisableAutoCommit(),
		kgo.SessionTimeout(45*time.Second),
		kgo.HeartbeatInterval(3*time.Second),
		kgo.MetadataMaxAge(10*time.Second),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerBatchMaxBytes(16*1024*1024),
		// On KRaft brokers (cp-kafka 7.6.1+), producing to a not-yet-existent
		// topic hangs ProduceSync forever unless the request itself flags
		// auto-creation, even with broker-side auto.create.topics.enable=true.
		kgo.AllowAutoTopicCreation(),
	)
	if err != nil {
		log.Fatalf("kgo.NewClient: %v", err) //nolint:gocritic
	}
	defer client.Close()

	// /healthz + /metrics-placeholder so compose healthchecks work.
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
		srv := &http.Server{Addr: metricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("health server: %v", err)
		}
	}()

	for rootCtx.Err() == nil {
		if err := runOnce(rootCtx, client, m); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				break
			}
			log.Printf("loop error, backoff 1s: %v", err)
			select {
			case <-rootCtx.Done():
			case <-time.After(time.Second):
			}
		}
	}
	log.Printf("transformer shutting down")
}

// runOnce drains one poll batch, transforms each record, produces to
// transformed.<suffix>, and commits offsets of every record that produced
// successfully. Returns the first apply/produce error so the caller can
// decide whether to back off.
func runOnce(ctx context.Context, client *kgo.Client, m *mapper.Mapper) error {
	fetches := client.PollFetches(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errs := fetches.Errors(); len(errs) > 0 {
		return errs[0].Err
	}

	var firstErr error
	var committable []*kgo.Record

	fetches.EachRecord(func(r *kgo.Record) {
		if firstErr != nil {
			return
		}
		// Tombstone: forward unchanged (sink recognises nil value).
		if r.Value == nil {
			out := &kgo.Record{Topic: targetTopic(r.Topic), Key: r.Key, Value: nil}
			if err := client.ProduceSync(ctx, out).FirstErr(); err != nil {
				firstErr = err
				return
			}
			committable = append(committable, r)
			return
		}
		transformed, err := m.ApplyJSON(r.Topic, r.Value)
		if err != nil {
			firstErr = err
			return
		}
		out := &kgo.Record{Topic: targetTopic(r.Topic), Key: r.Key, Value: transformed}
		if err := client.ProduceSync(ctx, out).FirstErr(); err != nil {
			firstErr = err
			return
		}
		committable = append(committable, r)
	})

	if len(committable) > 0 {
		client.MarkCommitRecords(committable...)
		if err := client.CommitMarkedOffsets(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// targetTopic maps "cdc.users" → "transformed.users". Any unknown prefix
// gets "transformed." prepended so pass-through still works for future
// topic naming schemes.
func targetTopic(source string) string {
	if rest, ok := strings.CutPrefix(source, "cdc."); ok {
		return "transformed." + rest
	}
	return "transformed." + source
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
