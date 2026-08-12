package events

import (
	"context"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/jusassessoria/platform/lib/obs"
)

// asynqQueueDepth is the observable gauge name: tasks waiting/running in each asynq
// queue. It is THE operational signal for the import — a growing pending count means the
// consumers are not keeping up with the relay/scheduler producers.
const asynqQueueDepth = "asynq.queue.depth"

// RegisterQueueDepth wires an observable gauge reporting asynq queue depth per queue and
// state (pending|active|scheduled|retry|archived), sampled by the metric reader. Call it
// ONCE from a single process (the relay is a singleton) so the series is not duplicated
// across scaled worker replicas — every worker sees the same shared Redis counts. It
// reads whatever queues exist at collection time (Inspector.Queues), so a new domain
// queue appears without a code change. The caller owns inspector's lifecycle (Close).
func RegisterQueueDepth(inspector *asynq.Inspector) error {
	meter := obs.Meter()
	gauge, err := meter.Int64ObservableGauge(
		asynqQueueDepth,
		metric.WithDescription("Tasks in an asynq queue, by state (pending/active/scheduled/retry/archived)."),
	)
	if err != nil {
		return err
	}

	_, err = meter.RegisterCallback(
		func(_ context.Context, o metric.Observer) error {
			queues, err := inspector.Queues()
			if err != nil {
				return err
			}
			for _, q := range queues {
				info, err := inspector.GetQueueInfo(q)
				if err != nil {
					continue // a queue that vanished mid-scan just reports nothing this tick
				}
				observeQueueDepth(o, gauge, info)
			}
			return nil
		},
		gauge,
	)
	return err
}

// observeQueueDepth emits one measurement per state for a queue's current info.
func observeQueueDepth(o metric.Observer, gauge metric.Int64ObservableGauge, info *asynq.QueueInfo) {
	byState := [...]struct {
		state string
		n     int
	}{
		{"pending", info.Pending},
		{"active", info.Active},
		{"scheduled", info.Scheduled},
		{"retry", info.Retry},
		{"archived", info.Archived},
	}
	for _, s := range byState {
		o.ObserveInt64(gauge, int64(s.n), metric.WithAttributes(
			attribute.String("queue", info.Queue),
			attribute.String("state", s.state),
		))
	}
}
