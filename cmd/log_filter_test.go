package main

import (
	"errors"
	"testing"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestFilterCertRotationConflictsSuppressesExpectedConflict(t *testing.T) {
	sink := newRecordingSink()
	logger := filterCertRotationConflicts(logr.New(sink))

	logger.WithName("cert-rotation").Error(
		newSecretConflict(),
		"could not refresh CA and server certs",
	)

	if len(*sink.errors) != 0 {
		t.Fatalf("expected conflict log to be suppressed, got %d entries", len(*sink.errors))
	}
}

func TestFilterCertRotationConflictsLogsOtherErrors(t *testing.T) {
	tests := []struct {
		name       string
		loggerName string
		err        error
		msg        string
	}{
		{
			name:       "same message but not conflict",
			loggerName: "cert-rotation",
			err:        errors.New("permission denied"),
			msg:        "could not refresh CA and server certs",
		},
		{
			name:       "same conflict but different logger",
			loggerName: "setup",
			err:        newSecretConflict(),
			msg:        "could not refresh CA and server certs",
		},
		{
			name:       "same conflict but different message",
			loggerName: "cert-rotation",
			err:        newSecretConflict(),
			msg:        "could not refresh cert on startup",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := newRecordingSink()
			logger := filterCertRotationConflicts(logr.New(sink))

			logger.WithName(tt.loggerName).Error(tt.err, tt.msg)

			if len(*sink.errors) != 1 {
				t.Fatalf("expected error log to pass through, got %d entries", len(*sink.errors))
			}
			if (*sink.errors)[0].msg != tt.msg {
				t.Fatalf("expected message %q, got %q", tt.msg, (*sink.errors)[0].msg)
			}
		})
	}
}

type recordedError struct {
	err error
	msg string
}

type recordingSink struct {
	errors *[]recordedError
	name   string
}

func newRecordingSink() *recordingSink {
	records := []recordedError{}
	return &recordingSink{errors: &records}
}

func newSecretConflict() error {
	return apierrors.NewConflict(
		schema.GroupResource{Resource: "secrets"},
		"webhook-server-cert",
		errors.New("modified"),
	)
}

func (r *recordingSink) Init(logr.RuntimeInfo) {}

func (r *recordingSink) Enabled(int) bool {
	return true
}

func (r *recordingSink) Info(int, string, ...any) {}

func (r *recordingSink) Error(err error, msg string, _ ...any) {
	*r.errors = append(*r.errors, recordedError{err: err, msg: msg})
}

func (r *recordingSink) WithValues(...any) logr.LogSink {
	return r
}

func (r *recordingSink) WithName(name string) logr.LogSink {
	return &recordingSink{
		errors: r.errors,
		name:   appendLoggerName(r.name, name),
	}
}
