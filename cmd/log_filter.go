package main

import (
	"strings"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	certRotationLoggerName          = "cert-rotation"
	malformedSecretWebhookConfigMsg = "secret is not well-formed, cannot update webhook configurations"
	missingCACertSecretErr          = "Cert secret is not well-formed, missing ca.crt"
	missingCAKeySecretErr           = "Cert secret is not well-formed, missing ca.key"
)

var suppressibleCertRotationConflictMessages = map[string]struct{}{
	"could not refresh CA and server certs": {},
	"could not refresh server certs":        {},
}

type certRotationConflictFilter struct {
	logger    logr.Logger
	name      string
	callDepth int
}

func filterCertRotationConflicts(logger logr.Logger) logr.Logger {
	return logr.New(&certRotationConflictFilter{logger: logger})
}

func (f *certRotationConflictFilter) Init(logr.RuntimeInfo) {}

func (f *certRotationConflictFilter) Enabled(level int) bool {
	return f.logger.V(level).Enabled()
}

func (f *certRotationConflictFilter) Info(level int, msg string, keysAndValues ...any) {
	f.logger.V(level).WithCallDepth(f.callDepth+1).Info(msg, keysAndValues...)
}

func (f *certRotationConflictFilter) Error(err error, msg string, keysAndValues ...any) {
	if f.suppresses(err, msg) {
		return
	}
	f.logger.WithCallDepth(f.callDepth+1).Error(err, msg, keysAndValues...)
}

func (f *certRotationConflictFilter) WithValues(keysAndValues ...any) logr.LogSink {
	return &certRotationConflictFilter{
		logger:    f.logger.WithValues(keysAndValues...),
		name:      f.name,
		callDepth: f.callDepth,
	}
}

func (f *certRotationConflictFilter) WithName(name string) logr.LogSink {
	return &certRotationConflictFilter{
		logger:    f.logger.WithName(name),
		name:      appendLoggerName(f.name, name),
		callDepth: f.callDepth,
	}
}

func (f *certRotationConflictFilter) WithCallDepth(depth int) logr.LogSink {
	return &certRotationConflictFilter{
		logger:    f.logger,
		name:      f.name,
		callDepth: f.callDepth + depth,
	}
}

func (f *certRotationConflictFilter) suppresses(err error, msg string) bool {
	if !isCertRotationLogger(f.name) {
		return false
	}
	if isSuppressibleCertRotationConflict(err, msg) {
		return true
	}
	return isSuppressibleMalformedSecret(err, msg)
}

func isSuppressibleCertRotationConflict(err error, msg string) bool {
	if !apierrors.IsConflict(err) {
		return false
	}
	_, ok := suppressibleCertRotationConflictMessages[msg]
	return ok
}

func isSuppressibleMalformedSecret(err error, msg string) bool {
	if msg != malformedSecretWebhookConfigMsg || err == nil {
		return false
	}
	errMsg := err.Error()
	return errMsg == missingCACertSecretErr || errMsg == missingCAKeySecretErr
}

func isCertRotationLogger(name string) bool {
	return name == certRotationLoggerName || strings.HasSuffix(name, "/"+certRotationLoggerName)
}

func appendLoggerName(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}
