// Package logger owns bounded, redacted file diagnostics. The slog/file basics
// follow knot/internal/logger/logger.go at e0b4d51eea6647e192371059381039b99fb301a2;
// instance ownership, redaction and rotation are implemented here.
package logger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const DefaultMaxBytes int64 = 10 << 20
const DefaultHistory = 3

type Options struct {
	Level    slog.Level
	MaxBytes int64
	History  int
}
type File struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	size     int64
	max      int64
	history  int
	closed   bool
	failure  error
	logger   *slog.Logger
	redactor *Redactor
	level    slog.LevelVar
}

func Open(path string, opts Options) (*File, error) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.History <= 0 {
		opts.History = DefaultHistory
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if err := secureDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f := &File{path: path, max: opts.MaxBytes, history: opts.History, redactor: NewRedactor()}
	if err := f.open(); err != nil {
		return nil, err
	}
	f.level.Set(opts.Level)
	base := slog.NewJSONHandler(f, &slog.HandlerOptions{Level: &f.level})
	f.logger = slog.New(&redactingHandler{next: base, r: f.redactor})
	return f, nil
}
func (f *File) open() error {
	info, err := os.Lstat(f.path)
	if err == nil && !info.Mode().IsRegular() {
		return errors.New("log path must be a regular file")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = secureFile(file); err != nil {
		_ = file.Close()
		return err
	}
	info, err = file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	f.file = file
	f.size = info.Size()
	return nil
}
func (f *File) SetLevel(level slog.Level) { f.level.Set(level) }
func (f *File) Logger() *slog.Logger      { return f.logger }
func (f *File) Redactor() *Redactor       { return f.redactor }
func (f *File) Write(data []byte) (n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	defer func() {
		if err != nil && f.failure == nil && !f.closed {
			f.failure = err
		}
	}()
	requested := len(data)
	if f.closed {
		return 0, os.ErrClosed
	}
	if f.file == nil {
		return 0, errors.New("log writer unavailable")
	}
	// Bound a single record as well as files. Keep JSON valid by emitting a small
	// replacement instead of truncating a JSON string in the middle of a rune.
	if int64(len(data)) > f.max {
		data = []byte("{\"msg\":\"log record exceeded size limit\"}\n")
		if int64(len(data)) > f.max {
			return 0, errors.New("log size limit too small")
		}
	}
	original := requested
	if f.size+int64(len(data)) > f.max {
		if err = f.rotate(); err != nil {
			return 0, err
		}
	}
	n, err = f.file.Write(data)
	f.size += int64(n)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		return original, nil
	}
	return n, err
}
func (f *File) rotate() error {
	if err := f.file.Close(); err != nil {
		return err
	}
	f.file = nil
	oldest := fmt.Sprintf("%s.%d", f.path, f.history)
	if err := os.Remove(oldest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for i := f.history - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", f.path, i)
		to := fmt.Sprintf("%s.%d", f.path, i+1)
		if err := os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(f.path, f.path+".1"); err != nil {
		return err
	}
	return f.open()
}
func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return f.failure
	}
	f.closed = true
	if f.file != nil {
		f.failure = errors.Join(f.failure, f.file.Sync(), f.file.Close())
		f.file = nil
	}
	return f.failure
}
func (f *File) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed && f.failure == nil {
		return os.ErrClosed
	}
	return f.failure
}

// Redact at Handle time, including attributes introduced by With/WithGroup.
// With attributes can contain a secret registered by a later config load.
type handlerStep struct {
	attrs []slog.Attr
	group string
}
type redactingHandler struct {
	next  slog.Handler
	r     *Redactor
	steps []handlerStep
}

func (h *redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}
func (h *redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	out := slog.NewRecord(record.Time, record.Level, h.r.Text(record.Message), record.PC)
	next := h.next
	for _, step := range h.steps {
		if step.group != "" {
			next = next.WithGroup(step.group)
			continue
		}
		attrs := make([]slog.Attr, len(step.attrs))
		for i, a := range step.attrs {
			attrs[i] = redactAttr(h.r, a)
		}
		next = next.WithAttrs(attrs)
	}
	record.Attrs(func(a slog.Attr) bool { out.AddAttrs(redactAttr(h.r, a)); return true })
	// Fixed metadata remains readable even when all untrusted text is suppressed.
	if h.r.Saturated() {
		out.AddAttrs(slog.String("redaction_status", "saturated"))
	}
	return next.Handle(ctx, out)
}
func redactAttr(r *Redactor, a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	if sensitive(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	if a.Value.Kind() == slog.KindGroup {
		attrs := a.Value.Group()
		out := make([]slog.Attr, len(attrs))
		for i, a := range attrs {
			out[i] = redactAttr(r, a)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}
	if a.Value.Kind() == slog.KindString {
		return slog.String(a.Key, r.Text(a.Value.String()))
	}
	if a.Value.Kind() == slog.KindAny {
		return slog.Any(a.Key, r.Value(a.Key, a.Value.Any()))
	}
	return a
}
func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.steps = append(append([]handlerStep(nil), h.steps...), handlerStep{attrs: append([]slog.Attr(nil), attrs...)})
	return &next
}
func (h *redactingHandler) WithGroup(group string) slog.Handler {
	if group == "" {
		return h
	}
	next := *h
	next.steps = append(append([]handlerStep(nil), h.steps...), handlerStep{group: group})
	return &next
}

// Diagnostic records the minimum lifecycle/error trail even when legacy
// log_level=error suppresses ordinary info/debug logs. Handle still applies the
// same redaction, ownership and bounded writer as all other logs.
func Diagnostic(level slog.Level, message string, args ...any) {
	record := slog.NewRecord(time.Now(), level, message, 0)
	record.Add(args...)
	_ = slog.Default().Handler().Handle(context.Background(), record)
}
