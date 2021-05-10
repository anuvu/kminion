package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudhut/kminion/v2/kafka"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/zap"
)

type Service struct {
	// General
	config Config
	logger *zap.Logger

	kafkaSvc *kafka.Service // creates kafka client for us
	client   *kgo.Client

	// Service
	minionID               string  // unique identifier, reported in metrics, in case multiple instances run at the same time
	lastRoundtripTimestamp float64 // creation time (in utc ms) of the message that most recently passed the roundtripSla check

	// Metrics
	endToEndMessagesProduced  prometheus.Counter
	endToEndMessagesAcked     prometheus.Counter
	endToEndMessagesReceived  prometheus.Counter
	endToEndMessagesCommitted prometheus.Counter

	endToEndAckLatency       prometheus.Histogram
	endToEndRoundtripLatency prometheus.Histogram
	endToEndCommitLatency    prometheus.Histogram
}

// NewService creates a new instance of the e2e moinitoring service (wow)
func NewService(cfg Config, logger *zap.Logger, kafkaSvc *kafka.Service, metricNamespace string, ctx context.Context) (*Service, error) {

	client, err := createKafkaClient(cfg, logger, kafkaSvc, ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create kafka client for e2e: %w", err)
	}

	svc := &Service{
		config:   cfg,
		logger:   logger.With(zap.String("source", "end_to_end")),
		kafkaSvc: kafkaSvc,
		client:   client,

		minionID: uuid.NewString(),
	}

	makeCounter := func(name string, help string) prometheus.Counter {
		return promauto.NewCounter(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: "end_to_end",
			Name:      name,
			Help:      help,
		})
	}
	makeHistogram := func(name string, maxLatency time.Duration, help string) prometheus.Histogram {
		return promauto.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: "end_to_end",
			Name:      name,
			Help:      help,
			Buckets:   createHistogramBuckets(maxLatency),
		})
	}

	// Low-level info
	// Users can construct alerts like "can't produce messages" themselves from those
	svc.endToEndMessagesProduced = makeCounter("messages_produced_total", "Number of messages that kminion's end-to-end test has tried to send to kafka")
	svc.endToEndMessagesAcked = makeCounter("messages_acked_total", "Number of messages kafka acknowledged as produced")
	svc.endToEndMessagesReceived = makeCounter("messages_received_total", "Number of *matching* messages kminion received. Every roundtrip message has a minionID (randomly generated on startup) and a timestamp. Kminion only considers a message a match if it it arrives within the configured roundtrip SLA (and it matches the minionID)")
	svc.endToEndMessagesCommitted = makeCounter("messages_committed_total", "Number of *matching* messages kminion successfully commited as read/processed. See 'messages_received' for what 'matching' means. Kminion will commit late/mismatching messages to kafka as well, but those won't be counted in this metric.")

	// Latency Histograms
	// More detailed info about how long stuff took
	// Since histograms also have an 'infinite' bucket, they can be used to detect small hickups "lost" messages
	svc.endToEndAckLatency = makeHistogram("produce_latency_seconds", cfg.Producer.AckSla, "Time until we received an ack for a produced message")
	svc.endToEndRoundtripLatency = makeHistogram("roundtrip_latency_seconds", cfg.Consumer.RoundtripSla, "Time it took between sending (producing) and receiving (consuming) a message")
	svc.endToEndCommitLatency = makeHistogram("commit_latency_seconds", cfg.Consumer.CommitSla, "Time kafka took to respond to kminion's offset commit")

	return svc, nil
}

func createKafkaClient(cfg Config, logger *zap.Logger, kafkaSvc *kafka.Service, ctx context.Context) (*kgo.Client, error) {

	// Add RequiredAcks, as options can't be altered later
	kgoOpts := []kgo.Opt{}
	if cfg.Enabled {
		ack := kgo.AllISRAcks()
		if cfg.Producer.RequiredAcks == 1 {
			ack = kgo.LeaderAck()
			kgoOpts = append(kgoOpts, kgo.DisableIdempotentWrite())
		}
		kgoOpts = append(kgoOpts, kgo.RequiredAcks(ack))
	}

	// Prepare hooks
	e2eHooks := newEndToEndClientHooks(logger)
	kgoOpts = append(kgoOpts, kgo.WithHooks(e2eHooks))

	// Create kafka service and check if client can successfully connect to Kafka cluster
	return kafkaSvc.CreateAndTestClient(logger, kgoOpts, ctx)
}

// Start starts the service (wow)
func (s *Service) Start(ctx context.Context) error {

	if err := s.validateManagementTopic(ctx); err != nil {
		return fmt.Errorf("could not validate end-to-end topic: %w", err)
	}

	go s.initEndToEnd(ctx)

	return nil
}

// called from e2e when a message is acknowledged
func (s *Service) onAck(partitionId int32, duration time.Duration) {
	s.endToEndMessagesAcked.Inc()
	s.endToEndAckLatency.Observe(duration.Seconds())
}

// called from e2e when a message completes a roundtrip (send to kafka, receive msg from kafka again)
func (s *Service) onRoundtrip(partitionId int32, duration time.Duration) {
	if duration > s.config.Consumer.RoundtripSla {
		return // message is too old
	}

	// todo: track "lastRoundtripMessage"
	// if msg.Timestamp < s.lastRoundtripTimestamp {
	// 	return // msg older than what we recently processed (out of order, should never happen)
	// }

	s.endToEndMessagesReceived.Inc()
	s.endToEndRoundtripLatency.Observe(duration.Seconds())
}

// called from e2e when an offset commit is confirmed
func (s *Service) onOffsetCommit(partitionId int32, duration time.Duration) {

	// todo:
	// if the commit took too long, don't count it in 'commits' but add it to the histogram?
	// and how do we want to handle cases where we get an error??
	// should we have another metric that tells us about failed commits? or a label on the counter?

	s.endToEndCommitLatency.Observe(duration.Seconds())

	if duration > s.config.Consumer.CommitSla {
		return
	}

	s.endToEndMessagesCommitted.Inc()
}