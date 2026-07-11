package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

type hostCallStep struct {
	method string
	result any
	err    error
}

type scriptedHost struct {
	t     *testing.T
	steps []hostCallStep
	calls []string
}

func (s *scriptedHost) call(method string, _ any) (json.RawMessage, error) {
	s.t.Helper()
	s.calls = append(s.calls, method)
	if len(s.steps) == 0 {
		s.t.Fatalf("unexpected host call %q", method)
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	if method != step.method {
		s.t.Fatalf("host call = %q, want %q", method, step.method)
	}
	if step.err != nil {
		return nil, step.err
	}
	raw, err := json.Marshal(step.result)
	if err != nil {
		s.t.Fatalf("marshal host result for %q: %v", method, err)
	}
	return raw, nil
}

func (s *scriptedHost) assertDone() {
	s.t.Helper()
	if len(s.steps) != 0 {
		s.t.Fatalf("%d scripted host calls were not consumed", len(s.steps))
	}
}

func retryTestFoldState(host *scriptedHost, sleeps *[]time.Duration, emit streamEmitFunc) *foldState {
	fs := newFoldState(
		map[string]any{"model": "gpt-5.6", "input": []any{}},
		[]any{},
		rpcExecutorRequest{},
		"callback-1",
	)
	fs.config = defaultFoldConfig()
	fs.hostCall = host.call
	fs.sleep = func(delay time.Duration) {
		*sleeps = append(*sleeps, delay)
	}
	if emit != nil {
		fs.streamEmit = emit
	}
	return fs
}

func streamStep(streamID string) hostCallStep {
	return hostCallStep{
		method: pluginabi.MethodHostModelExecuteStream,
		result: hostModelStreamResponse{StatusCode: http.StatusOK, StreamID: streamID},
	}
}

func readStep(payload string) hostCallStep {
	return hostCallStep{
		method: pluginabi.MethodHostModelStreamRead,
		result: hostModelStreamReadResponse{Payload: []byte(payload)},
	}
}

func readErrorStep(message string) hostCallStep {
	return hostCallStep{
		method: pluginabi.MethodHostModelStreamRead,
		result: hostModelStreamReadResponse{Error: message, Done: true},
	}
}

func closeStep() hostCallStep {
	return hostCallStep{method: pluginabi.MethodHostModelStreamClose, result: map[string]any{}}
}

func TestOpenRoundRetriesTransientStartupFailure(t *testing.T) {
	host := &scriptedHost{t: t, steps: []hostCallStep{
		{
			method: pluginabi.MethodHostModelExecuteStream,
			err:    errors.New(`host_call_failed: Post "http://172.18.0.1:8080/responses": dial tcp 172.18.0.1:8080: connect: connection refused`),
		},
		streamStep("stream-2"),
		readStep(`data: {"type":"response.completed","response":{"status":"completed"}}`),
		closeStep(),
	}}
	var sleeps []time.Duration
	fs := retryTestFoldState(host, &sleeps, nil)

	terminal, _, _, err := fs.openRound("downstream-1")
	if err != nil {
		t.Fatalf("openRound() error = %v", err)
	}
	if terminal == nil || terminal["type"] != "response.completed" {
		t.Fatalf("terminal = %#v, want response.completed", terminal)
	}
	if !reflect.DeepEqual(sleeps, []time.Duration{500 * time.Millisecond}) {
		t.Fatalf("sleeps = %v, want [500ms]", sleeps)
	}
	if fs.roundNo != 1 {
		t.Fatalf("roundNo = %d, retry must not count as another fold round", fs.roundNo)
	}
	host.assertDone()
}

func TestOpenRoundUsesBoundedExponentialBackoff(t *testing.T) {
	host := &scriptedHost{t: t, steps: []hostCallStep{
		{method: pluginabi.MethodHostModelExecuteStream, err: errors.New("connection refused")},
		{method: pluginabi.MethodHostModelExecuteStream, err: errors.New("connection reset by peer")},
		{method: pluginabi.MethodHostModelExecuteStream, err: errors.New("Service temporarily unavailable")},
		{method: pluginabi.MethodHostModelExecuteStream, err: errors.New("bad gateway")},
	}}
	var sleeps []time.Duration
	fs := retryTestFoldState(host, &sleeps, nil)

	_, _, _, err := fs.openRound("downstream-1")
	if err == nil || err.Error() != "bad gateway" {
		t.Fatalf("openRound() error = %v, want final bad gateway error", err)
	}
	wantSleeps := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}
	if !reflect.DeepEqual(sleeps, wantSleeps) {
		t.Fatalf("sleeps = %v, want %v", sleeps, wantSleeps)
	}
	host.assertDone()
}

func TestOpenRoundRetriesTransientResponseFailedBeforeFirstEvent(t *testing.T) {
	host := &scriptedHost{t: t, steps: []hostCallStep{
		streamStep("stream-1"),
		readStep(`data: {"type":"response.failed","response":{"status":"failed","error":{"code":503,"message":"Service temporarily unavailable"}}}`),
		closeStep(),
		streamStep("stream-2"),
		readStep(`data: {"type":"response.completed","response":{"status":"completed"}}`),
		closeStep(),
	}}
	var sleeps []time.Duration
	emits := 0
	fs := retryTestFoldState(host, &sleeps, func(string, []byte) error {
		emits++
		return nil
	})

	terminal, _, _, err := fs.openRound("downstream-1")
	if err != nil {
		t.Fatalf("openRound() error = %v", err)
	}
	if terminal == nil || terminal["type"] != "response.completed" {
		t.Fatalf("terminal = %#v, want response.completed", terminal)
	}
	if emits != 0 {
		t.Fatalf("emits = %d, response.failed must be retried before it is emitted", emits)
	}
	if !reflect.DeepEqual(sleeps, []time.Duration{500 * time.Millisecond}) {
		t.Fatalf("sleeps = %v, want [500ms]", sleeps)
	}
	host.assertDone()
}

func TestOpenRoundRetriesTransientReadFailureBeforeFirstEvent(t *testing.T) {
	host := &scriptedHost{t: t, steps: []hostCallStep{
		streamStep("stream-1"),
		readErrorStep("connection reset by peer"),
		closeStep(),
		streamStep("stream-2"),
		readStep(`data: {"type":"response.completed","response":{"status":"completed"}}`),
		closeStep(),
	}}
	var sleeps []time.Duration
	fs := retryTestFoldState(host, &sleeps, nil)

	terminal, _, _, err := fs.openRound("downstream-1")
	if err != nil {
		t.Fatalf("openRound() error = %v", err)
	}
	if terminal == nil || terminal["type"] != "response.completed" {
		t.Fatalf("terminal = %#v, want response.completed", terminal)
	}
	if !reflect.DeepEqual(sleeps, []time.Duration{500 * time.Millisecond}) {
		t.Fatalf("sleeps = %v, want [500ms]", sleeps)
	}
	host.assertDone()
}

func TestOpenRoundDoesNotRetryAfterDownstreamEvent(t *testing.T) {
	host := &scriptedHost{t: t, steps: []hostCallStep{
		streamStep("stream-1"),
		readStep(`data: {"type":"response.created","response":{"id":"resp-1"}}`),
		readErrorStep("connection reset by peer"),
		closeStep(),
	}}
	var sleeps []time.Duration
	emits := 0
	fs := retryTestFoldState(host, &sleeps, func(string, []byte) error {
		emits++
		return nil
	})

	_, _, _, err := fs.openRound("downstream-1")
	if err == nil || err.Error() != "connection reset by peer" {
		t.Fatalf("openRound() error = %v, want connection reset", err)
	}
	if emits != 1 || !fs.downstreamStarted {
		t.Fatalf("emits = %d downstreamStarted = %t, want one started event", emits, fs.downstreamStarted)
	}
	if len(sleeps) != 0 {
		t.Fatalf("sleeps = %v, must not retry after emitting", sleeps)
	}
	host.assertDone()
}

func TestOpenRoundDoesNotRetryDownstreamEmitFailure(t *testing.T) {
	host := &scriptedHost{t: t, steps: []hostCallStep{
		streamStep("stream-1"),
		readStep(`data: {"type":"response.created","response":{"id":"resp-1"}}`),
		closeStep(),
	}}
	var sleeps []time.Duration
	fs := retryTestFoldState(host, &sleeps, func(string, []byte) error {
		return errors.New("connection reset by peer")
	})

	_, _, _, err := fs.openRound("downstream-1")
	if err == nil || err.Error() != "connection reset by peer" {
		t.Fatalf("openRound() error = %v, want downstream emit error", err)
	}
	if len(sleeps) != 0 {
		t.Fatalf("sleeps = %v, downstream emit failure must not retry upstream", sleeps)
	}
	host.assertDone()
}

func TestOpenRoundDoesNotRetryPermanentFailure(t *testing.T) {
	host := &scriptedHost{t: t, steps: []hostCallStep{
		{method: pluginabi.MethodHostModelExecuteStream, err: errors.New("invalid request: missing input")},
	}}
	var sleeps []time.Duration
	fs := retryTestFoldState(host, &sleeps, nil)

	_, _, _, err := fs.openRound("downstream-1")
	if err == nil || err.Error() != "invalid request: missing input" {
		t.Fatalf("openRound() error = %v, want permanent error", err)
	}
	if len(sleeps) != 0 {
		t.Fatalf("sleeps = %v, permanent failure must not retry", sleeps)
	}
	host.assertDone()
}

func TestTransientStartupErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "502", err: &upstreamError{status: 502, msg: "upstream returned status 502"}, want: true},
		{name: "503", err: &upstreamError{status: 503, msg: "upstream returned status 503"}, want: true},
		{name: "504", err: &upstreamError{status: 504, msg: "upstream returned status 504"}, want: true},
		{name: "500", err: &upstreamError{status: 500, msg: "internal error"}, want: false},
		{name: "api key startup window", err: errors.New(`host_call_failed: {"code":"INTERNAL_ERROR","message":"Failed to validate API key"}`), want: true},
		{name: "connection refused", err: errors.New(`host_call_failed: Post "http://172.18.0.1:8080/responses": dial tcp 172.18.0.1:8080: connect: connection refused`), want: true},
		{name: "temporary unavailable", err: errors.New(`host_call_failed: {"error":{"message":"Service temporarily unavailable","type":"api_error"}}`), want: true},
		{name: "status in message", err: errors.New(`host_call_failed: upstream status_code=504`), want: true},
		{name: "permanent invalid key", err: errors.New("invalid API key"), want: false},
		{name: "downstream connection reset", err: &downstreamEmitError{cause: errors.New("connection reset")}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientStartupError(tt.err); got != tt.want {
				t.Fatalf("isTransientStartupError(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}

func TestRetryBackoffCapsAtConfiguredMaximum(t *testing.T) {
	cfg := defaultFoldConfig()
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second}
	for index, expected := range want {
		if got := retryBackoff(cfg, index); got != expected {
			t.Fatalf("retryBackoff(index=%d) = %s, want %s", index, got, expected)
		}
	}
}

func TestDefaultStartupRetryConfig(t *testing.T) {
	cfg := defaultFoldConfig()
	if cfg.MaxStartupRetries != 3 || cfg.RetryInitialBackoffMS != 500 || cfg.RetryMaxBackoffMS != 2000 {
		t.Fatalf("unexpected startup retry defaults: %+v", cfg)
	}
}

func TestOpenRoundStartupRetriesCanBeDisabled(t *testing.T) {
	host := &scriptedHost{t: t, steps: []hostCallStep{
		{method: pluginabi.MethodHostModelExecuteStream, err: errors.New("connection refused")},
	}}
	var sleeps []time.Duration
	fs := retryTestFoldState(host, &sleeps, nil)
	fs.config.MaxStartupRetries = 0

	_, _, _, err := fs.openRound("downstream-1")
	if err == nil || err.Error() != "connection refused" {
		t.Fatalf("openRound() error = %v, want connection refused", err)
	}
	if len(sleeps) != 0 {
		t.Fatalf("sleeps = %v, retries are disabled", sleeps)
	}
	host.assertDone()
}
