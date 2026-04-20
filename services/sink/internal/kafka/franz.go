// Package kafka adapts franz-go's kgo.Client to the consumer.KafkaConsumer
// interface. The wiring is intentionally thin — no logic lives here. Logic
// is in the Loop (consumer) and the Writer; this package only translates
// between franz-go types and our internal Record shape.
package kafka

import (
	"context"
	"fmt"
	"time"

	"zdt/sink/internal/consumer"

	"github.com/twmb/franz-go/pkg/kgo"
)

type FranzConsumer struct {
	client *kgo.Client
}

// New creates a franz-go client configured for manual-commit operation,
// matching the ADR-003 "commit after side-effect" invariant: AutoCommit is
// disabled, the Loop calls MarkCommit on success and CommitMarked at end
// of batch.
func New(brokers []string, groupID, topicRegex string) (*FranzConsumer, error) {
	client, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(groupID),
		kgo.ConsumeRegex(),
		kgo.ConsumeTopics(topicRegex),
		kgo.DisableAutoCommit(),
		kgo.SessionTimeout(45*time.Second),
		kgo.HeartbeatInterval(3*time.Second),
		// A pattern-subscribing consumer only picks up new topics on the
		// next metadata refresh. Shorten it so freshly-created cdc.*
		// topics are subscribed within seconds, not the 5m default —
		// otherwise a cold start race leaves the consumer with 0
		// partitions, exactly the Week-1 symptom we hit before.
		kgo.MetadataMaxAge(10*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("kafka.New: %w", err)
	}
	return &FranzConsumer{client: client}, nil
}

func (f *FranzConsumer) Close() {
	f.client.Close()
}

// Poll blocks up to a few seconds waiting for new records, returning an
// empty slice if the broker had nothing for us. Errors are surfaced only
// if every partition errored — transient per-partition errors are not
// propagated (franz-go retries them internally).
func (f *FranzConsumer) Poll(ctx context.Context) ([]consumer.Record, error) {
	fetches := f.client.PollFetches(ctx)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errs := fetches.Errors(); len(errs) > 0 {
		return nil, fmt.Errorf("kafka.Poll: %v", errs[0].Err)
	}
	var out []consumer.Record
	fetches.EachRecord(func(r *kgo.Record) {
		out = append(out, consumer.Record{
			Key:       r.Key,
			Value:     r.Value,
			Offset:    r.Offset,
			Partition: r.Partition,
			Topic:     r.Topic,
		})
	})
	return out, nil
}

func (f *FranzConsumer) MarkCommit(r consumer.Record) {
	// franz-go's MarkCommitOffsets expects "next offset to consume" = current + 1.
	f.client.MarkCommitOffsets(map[string]map[int32]kgo.EpochOffset{
		r.Topic: {
			r.Partition: kgo.EpochOffset{Offset: r.Offset + 1, Epoch: -1},
		},
	})
}

func (f *FranzConsumer) CommitMarked(ctx context.Context) error {
	if err := f.client.CommitMarkedOffsets(ctx); err != nil {
		return fmt.Errorf("kafka.CommitMarked: %w", err)
	}
	return nil
}
