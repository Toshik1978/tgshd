package zte

import "go.uber.org/zap"

// Logger implements resty Logger.
type logger struct {
	log *zap.SugaredLogger
}

// newLogger instantiate new Logger.
func newLogger(log *zap.Logger) *logger {
	return &logger{log: log.Sugar()}
}

func (l *logger) Errorf(format string, v ...any) {
	l.log.Errorf(format, v...)
}

func (l *logger) Warnf(format string, v ...any) {
	l.log.Warnf(format, v...)
}

func (l *logger) Debugf(format string, v ...any) {
	l.log.Debugf(format, v...)
}
