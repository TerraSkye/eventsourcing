package logging

import "log/slog"

// loggerOrDefault returns logger, or [slog.Default] if logger is nil.
func loggerOrDefault(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return slog.Default()
	}
	return logger
}
