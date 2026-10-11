// Package plugintest provides an in-memory implementation of plugin.Context
// for plugin unit tests; it records every operation a plugin performs so tests
// can assert on them.
package plugintest

import (
	extproc "github.com/JimDevil/ext-proc/sdk"

	"github.com/JimDevil/ext-proc/internal/plugin"
)

// HeaderOp records one header operation.
type HeaderOp struct {
	// Action is one of append / overwrite / remove.
	Action string
	Name   string
	Value  string
}

// MockContext is the in-memory plugin.Context implementation (tests only).
type MockContext struct {
	ops           []HeaderOp
	body          []byte
	bodySet       bool
	values        map[string]any
	eos           bool
	aborted       bool
	cancelStatus  int32
	cancelBody    string
	cancelHeaders map[string]extproc.HeaderValue
}

// NewMockContext builds an empty mock context.
func NewMockContext() *MockContext {
	return &MockContext{values: map[string]any{}}
}

// WithEndOfStream sets the EndOfStream return value (chainable).
func (m *MockContext) WithEndOfStream(v bool) *MockContext {
	m.eos = v
	return m
}

// HeaderOps returns all recorded header operations, in order.
func (m *MockContext) HeaderOps() []HeaderOp {
	return append([]HeaderOp(nil), m.ops...)
}

// LastHeaderValue returns the most recent set/append value for a header;
// false when the header was never operated on.
func (m *MockContext) LastHeaderValue(name string) (string, bool) {
	for i := len(m.ops) - 1; i >= 0; i-- {
		op := m.ops[i]
		if op.Action != "remove" && op.Name == name {
			return op.Value, true
		}
	}
	return "", false
}

// RemovedHeaders returns the names of removed headers.
func (m *MockContext) RemovedHeaders() []string {
	var out []string
	for _, op := range m.ops {
		if op.Action == "remove" {
			out = append(out, op.Name)
		}
	}
	return out
}

// Body returns the last ReplaceBody content; false when never replaced.
func (m *MockContext) Body() ([]byte, bool) {
	return m.body, m.bodySet
}

// CancelStatus returns the short-circuit status code; false when not aborted.
func (m *MockContext) CancelStatus() (int32, bool) {
	return m.cancelStatus, m.aborted
}

// CancelBody returns the short-circuit response body.
func (m *MockContext) CancelBody() string { return m.cancelBody }

// ---- plugin.Context implementation ----

func (m *MockContext) ContinueRequest() error { return nil }

func (m *MockContext) CancelRequest(status int32, headers map[string]extproc.HeaderValue, body string) error {
	m.aborted = true
	m.cancelStatus = status
	m.cancelBody = body
	m.cancelHeaders = headers
	return nil
}

func (m *MockContext) AppendHeader(name string, hv extproc.HeaderValue) error {
	m.ops = append(m.ops, HeaderOp{Action: "append", Name: name, Value: hv.Value})
	return nil
}

func (m *MockContext) OverwriteHeader(name string, hv extproc.HeaderValue) error {
	m.ops = append(m.ops, HeaderOp{Action: "overwrite", Name: name, Value: hv.Value})
	return nil
}

func (m *MockContext) RemoveHeader(name string) error {
	m.ops = append(m.ops, HeaderOp{Action: "remove", Name: name})
	return nil
}

func (m *MockContext) ReplaceBody(body []byte) error {
	m.body = body
	m.bodySet = true
	return nil
}

func (m *MockContext) EndOfStream() bool { return m.eos }

func (m *MockContext) SetValue(name string, val any) error {
	m.values[name] = val
	return nil
}

func (m *MockContext) GetValue(name string) (any, bool) {
	v, ok := m.values[name]
	return v, ok
}

func (m *MockContext) Aborted() bool { return m.aborted }

// Compile-time check that the mock implements the narrow plugin interface.
var _ plugin.Context = (*MockContext)(nil)
