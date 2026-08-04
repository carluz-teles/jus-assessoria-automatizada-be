package events

import (
	"context"
	"fmt"
	"log/slog"
)

// asynqSlog adapts asynq's Logger to slog so asynq's OWN operational logs (Redis
// faults, server lifecycle) are structured and reach OTLP/New Relic like every
// other log, instead of landing as unstructured stderr. The message stays a
// stable "asynq" (low cardinality for grouping); the freeform text asynq passes
// goes into a "detail" attribute.
//
// Per-task retry/failure logging is intentionally NOT done here — the Observe
// middleware owns it (one structured line per failure, with the correlation keys).
// Wire this together with asynq LogLevel=ErrorLevel (see NewAsynqLogger usage) so
// asynq's own Warn retry lines don't duplicate the middleware's failure log.
type asynqSlog struct{ logger *slog.Logger }

// NewAsynqLogger adapts logger for asynq.Config.Logger. Satisfies asynq.Logger.
func NewAsynqLogger(logger *slog.Logger) *asynqSlog {
	return &asynqSlog{logger: logger}
}

func (a *asynqSlog) Debug(args ...any) { a.log(slog.LevelDebug, args) }
func (a *asynqSlog) Info(args ...any)  { a.log(slog.LevelInfo, args) }
func (a *asynqSlog) Warn(args ...any)  { a.log(slog.LevelWarn, args) }
func (a *asynqSlog) Error(args ...any) { a.log(slog.LevelError, args) }

// Fatal maps to Error, not os.Exit: asynq only logs Fatal for unrecoverable setup
// faults, and the boot/health path already fails fast — crashing the process from
// a log adapter would bypass graceful shutdown.
func (a *asynqSlog) Fatal(args ...any) { a.log(slog.LevelError, args) }

func (a *asynqSlog) log(level slog.Level, args []any) {
	a.logger.LogAttrs(context.Background(), level, "asynq", slog.String("detail", fmt.Sprint(args...)))
}
