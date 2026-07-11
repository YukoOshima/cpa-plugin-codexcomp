package main

import (
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type rpcModelRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type hostModelStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}

type hostModelStreamReadResponse struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

const (
	cpaSessionHeader        = "X-CPA-Session-Id"
	codexCompSessionHeader  = "X-CodexComp-Session-Id"
	claudeCodeSessionHeader = "X-Claude-Code-Session-Id"
)

var sessionHeaders = []string{
	cpaSessionHeader,
	codexCompSessionHeader,
	claudeCodeSessionHeader,
}

const (
	gpt55TruncationFoldRouteReason = "codexcomp_gpt55_truncation_fold"
	gpt56TruncationFoldRouteReason = "codexcomp_gpt56_truncation_fold"
)

func supportsTruncationFold(model string) bool {
	if model == "gpt-5.5" || model == "gpt-5.6" {
		return true
	}
	const gpt56VariantPrefix = "gpt-5.6-"
	return len(model) > len(gpt56VariantPrefix) && strings.HasPrefix(model, gpt56VariantPrefix)
}

func truncationFoldRouteReason(model string) string {
	if model == "gpt-5.5" {
		return gpt55TruncationFoldRouteReason
	}
	return gpt56TruncationFoldRouteReason
}

func extractSessionID(req rpcExecutorRequest) string {
	for _, header := range sessionHeaders {
		if sid := strings.TrimSpace(req.Headers.Get(header)); sid != "" {
			return sid
		}
	}
	if len(req.OriginalRequest) == 0 {
		return ""
	}
	var payload struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(req.OriginalRequest, &payload); err != nil {
		return ""
	}
	userID := payload.Metadata.UserID
	if userID == "" {
		return ""
	}
	if strings.HasPrefix(userID, "{") {
		var uid struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal([]byte(userID), &uid); err == nil {
			return strings.TrimSpace(uid.SessionID)
		}
		return ""
	}
	if idx := strings.LastIndex(userID, "_session_"); idx >= 0 {
		return strings.TrimSpace(userID[idx+len("_session_"):])
	}
	return ""
}

func stablePromptCacheKey(model, sessionID string) string {
	name := strings.Join([]string{"codexcomp", "prompt-cache", model, "session:" + sessionID}, ":")
	h := sha1.Sum([]byte(name))
	return fmt.Sprintf("%x", h)
}

type streamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload"`
	Error    string `json:"error,omitempty"`
}

type streamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func emitChunk(streamID string, payload []byte) error {
	if streamID == "" || len(payload) == 0 {
		return nil
	}
	_, err := callHost(pluginabi.MethodHostStreamEmit, streamEmitRequest{
		StreamID: streamID,
		Payload:  payload,
	})
	return err
}

func closeStream(streamID, errMsg string) {
	if streamID == "" {
		return
	}
	_, _ = callHost(pluginabi.MethodHostStreamClose, streamCloseRequest{StreamID: streamID, Error: errMsg})
}

func routeModel(raw []byte) ([]byte, error) {
	var req rpcModelRouteRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode model.route request: %w", err)
	}

	if !supportsTruncationFold(req.RequestedModel) {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	switch req.SourceFormat {
	case "openai-response", "openai", "claude":
		// accepted: CPA adapter will translate to/from codex internally
	default:
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	if !req.Stream {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}

	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}

	if _, hasPRI := body["previous_response_id"]; hasPRI {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}

	if req.SourceFormat == "openai-response" {
		input, ok := body["input"]
		if !ok {
			return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
		}
		if _, isArray := input.([]any); !isArray {
			return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
		}
	}

	return okEnvelope(pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     truncationFoldRouteReason(req.RequestedModel),
	})
}

func execute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode executor.execute request: %w", err)
	}

	body := req.OriginalRequest
	if len(req.Payload) > 0 {
		body = req.Payload
	}

	entryProtocol := "codex"
	if sid := extractSessionID(req); sid != "" {
		var bodyMap map[string]any
		if err := json.Unmarshal(body, &bodyMap); err == nil {
			bodyMap["prompt_cache_key"] = stablePromptCacheKey(req.Model, sid)
			body, _ = json.Marshal(bodyMap)
			entryProtocol = "openai-response"
		}
	}

	result, err := callHost(pluginabi.MethodHostModelExecute, hostModelExecRequest{
		EntryProtocol:  entryProtocol,
		ExitProtocol:   "codex",
		Model:          req.Model,
		Stream:         false,
		Body:           body,
		Headers:        cloneHeader(req.Headers),
		Query:          req.Query,
		Alt:            req.Alt,
		HostCallbackID: req.HostCallbackID,
	})
	if err != nil {
		return errorEnvelope("executor_error", err.Error()), nil
	}

	var resp pluginapi.HostModelExecutionResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("decode host.model.execute result: %w", err)
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: resp.Body, Headers: resp.Headers})
}

func executeStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode executor.execute_stream request: %w", err)
	}

	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream"), nil
	}

	baseBody := map[string]any{}
	bodyBytes := req.OriginalRequest
	if len(req.Payload) > 0 {
		bodyBytes = req.Payload
	}
	if err := json.Unmarshal(bodyBytes, &baseBody); err != nil {
		closeStream(streamID, "decode request body: "+err.Error())
		return okEnvelope(map[string]any{
			"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
		})
	}

	sessionID := extractSessionID(req)
	if sessionID != "" {
		baseBody["prompt_cache_key"] = stablePromptCacheKey(req.Model, sessionID)
	}

	origInput, _ := baseBody["input"].([]any)
	if origInput == nil {
		origInput = []any{}
	} else {
		origInput = append([]any(nil), origInput...)
	}

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				closeStream(streamID, fmt.Sprintf("fold panic: %v", recovered))
			}
		}()
		runFold(baseBody, origInput, req, streamID)
		closeStream(streamID, "")
	}()

	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

func runFold(baseBody map[string]any, origInput []any, req rpcExecutorRequest, streamID string) {
	fs := newFoldState(baseBody, origInput, req, req.HostCallbackID)

	for {
		terminal, usage, _, roundErr := fs.openRound(streamID)

		if roundErr != nil {
			var fev map[string]any
			if _, isMid := roundErr.(*midStreamError); isMid {
				fev = fs.incompleteEvent("upstream_error")
			} else if fs.roundNo == 1 {
				status := 502
				if ue, ok := roundErr.(*upstreamError); ok {
					status = ue.status
				}
				fev = failedEvent(status, roundErr.Error())
			} else {
				fev = fs.incompleteEvent("upstream_error")
			}
			fs.stamp(fev)
			_ = fs.emitChunk(streamID, sseEvent(fev))
			_ = fs.emitDone(streamID)
			return
		}

		if terminal == nil {
			iev := fs.incompleteEvent("upstream_eof")
			fs.stamp(iev)
			_ = fs.emitChunk(streamID, sseEvent(iev))
			_ = fs.emitDone(streamID)
			return
		}

		fs.endRound(terminal, usage)

		if fs.shouldContinue() {
			fs.debugf("continuing after round=%d reasoning_tokens=%s", fs.roundNo, optionalIntString(reasoningTokens(fs.usage)))
			fs.prepareNextRound()
			continue
		}

		if err := fs.flushCleanStop(streamID); err != nil {
			_ = fs.emitDone(streamID)
			return
		}
		ev := fs.terminalEvent()
		fs.stamp(ev)
		if err := fs.emitChunk(streamID, sseEvent(ev)); err != nil {
			return
		}
		_ = fs.emitDone(streamID)
		return
	}
}

type upstreamError struct {
	status int
	msg    string
}

func (e *upstreamError) Error() string { return e.msg }

type midStreamError struct {
	msg   string
	cause error
}

func (e *midStreamError) Error() string { return e.msg }

func (e *midStreamError) Unwrap() error { return e.cause }

type downstreamEmitError struct{ cause error }

func (e *downstreamEmitError) Error() string { return e.cause.Error() }

func (e *downstreamEmitError) Unwrap() error { return e.cause }

func sseEvent(ev map[string]any) []byte {
	raw, _ := json.Marshal(ev)
	return append([]byte("data: "), append(raw, '\n', '\n')...)
}

type foldState struct {
	baseBody       map[string]any
	origInput      []any
	req            rpcExecutorRequest
	hostCallbackID string

	roundNo      int
	dsOI         int
	seq          int
	baseResponse map[string]any
	finalOutput  []map[string]any
	replayTail   []any
	summedUsage  map[string]any
	firstUsage   map[string]any
	roundsInfo   []map[string]any

	roundReasoning []map[string]any
	kind           map[int]string
	oiToDS         map[int]int
	buffered       []bufferedEntry
	terminal       map[string]any
	usage          map[string]any

	sseBuffer []byte
	config    foldConfig

	downstreamStarted bool
	hostCall          hostCallFunc
	streamEmit        streamEmitFunc
	sleep             sleepFunc
}

type hostCallFunc func(method string, payload any) (json.RawMessage, error)
type streamEmitFunc func(streamID string, payload []byte) error
type sleepFunc func(time.Duration)

type bufferedEntry struct {
	oi     int
	item   map[string]any
	events []map[string]any
}

func newFoldState(baseBody map[string]any, origInput []any, req rpcExecutorRequest, hostCallbackID string) *foldState {
	return &foldState{
		baseBody:       baseBody,
		origInput:      origInput,
		req:            req,
		hostCallbackID: hostCallbackID,
		summedUsage:    map[string]any{},
		kind:           map[int]string{},
		oiToDS:         map[int]int{},
		config:         currentFoldConfig(),
		hostCall:       callHost,
		streamEmit:     emitChunk,
		sleep:          time.Sleep,
	}
}

func (fs *foldState) openRound(streamID string) (map[string]any, map[string]any, http.Header, error) {
	fs.roundNo++
	retries := 0
	for {
		fs.resetRoundAttempt()
		terminal, usage, headers, roundErr := fs.openRoundAttempt(streamID)
		retryCause := roundErr
		if retryCause == nil {
			retryCause = terminalFailureError(terminal)
		}
		if retryCause == nil ||
			fs.downstreamStarted ||
			retries >= fs.config.MaxStartupRetries ||
			!isTransientStartupError(retryCause) {
			return terminal, usage, headers, roundErr
		}

		delay := retryBackoff(fs.config, retries)
		retries++
		fs.debugf(
			"retrying transient upstream startup failure retry=%d/%d delay=%s error=%v",
			retries,
			fs.config.MaxStartupRetries,
			delay,
			retryCause,
		)
		fs.sleepFor(delay)
	}
}

func (fs *foldState) resetRoundAttempt() {
	fs.roundReasoning = nil
	fs.kind = map[int]string{}
	fs.oiToDS = map[int]int{}
	fs.buffered = nil
	fs.terminal = nil
	fs.usage = nil
	fs.sseBuffer = nil
}

func (fs *foldState) openRoundAttempt(streamID string) (map[string]any, map[string]any, http.Header, error) {
	var bodyBytes []byte
	var err error
	bodyBytes, err = json.Marshal(nextRoundBody(fs.baseBody, append(fs.origInput, fs.replayTail...)))
	if err != nil {
		return nil, nil, nil, err
	}

	entryProtocol := "codex"
	if _, hasKey := fs.baseBody["prompt_cache_key"]; hasKey {
		entryProtocol = "openai-response"
	}

	result, err := fs.callHostMethod(pluginabi.MethodHostModelExecuteStream, hostModelExecRequest{
		EntryProtocol:  entryProtocol,
		ExitProtocol:   "codex",
		Model:          fs.req.Model,
		Stream:         true,
		Body:           bodyBytes,
		Headers:        cloneHeader(fs.req.Headers),
		Query:          fs.req.Query,
		Alt:            fs.req.Alt,
		HostCallbackID: fs.hostCallbackID,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	var streamResp hostModelStreamResponse
	if err := json.Unmarshal(result, &streamResp); err != nil {
		return nil, nil, nil, fmt.Errorf("decode host.model.execute_stream result: %w", err)
	}
	if streamResp.StatusCode >= 400 {
		if streamResp.StreamID != "" {
			_, _ = fs.callHostMethod(pluginabi.MethodHostModelStreamClose, map[string]any{"stream_id": streamResp.StreamID})
		}
		return nil, nil, nil, &upstreamError{status: streamResp.StatusCode, msg: fmt.Sprintf("upstream returned status %d", streamResp.StatusCode)}
	}
	if streamResp.StreamID == "" {
		return nil, nil, nil, fmt.Errorf("host.model.execute_stream returned empty stream_id")
	}
	defer func() {
		_, _ = fs.callHostMethod(pluginabi.MethodHostModelStreamClose, map[string]any{"stream_id": streamResp.StreamID})
	}()

	for {
		readResult, err := fs.callHostMethod(pluginabi.MethodHostModelStreamRead, map[string]any{"stream_id": streamResp.StreamID})
		if err != nil {
			return nil, nil, streamResp.Headers, &midStreamError{msg: err.Error(), cause: err}
		}
		var readResp hostModelStreamReadResponse
		if err := json.Unmarshal(readResult, &readResp); err != nil {
			return nil, nil, streamResp.Headers, &midStreamError{msg: err.Error(), cause: err}
		}
		if len(readResp.Payload) > 0 {
			term, perr := fs.processAndEmit(readResp.Payload, streamID)
			if perr != nil {
				return nil, nil, streamResp.Headers, &midStreamError{msg: perr.Error(), cause: perr}
			}
			if term != nil {
				return fs.terminal, fs.usage, streamResp.Headers, nil
			}
		}
		if readResp.Error != "" {
			return nil, nil, streamResp.Headers, &midStreamError{msg: readResp.Error}
		}
		if readResp.Done {
			return fs.terminal, fs.usage, streamResp.Headers, nil
		}
	}
}

func (fs *foldState) callHostMethod(method string, payload any) (json.RawMessage, error) {
	if fs.hostCall != nil {
		return fs.hostCall(method, payload)
	}
	return callHost(method, payload)
}

func (fs *foldState) emitChunk(streamID string, payload []byte) error {
	emitter := fs.streamEmit
	if emitter == nil {
		emitter = emitChunk
	}
	if err := emitter(streamID, payload); err != nil {
		return &downstreamEmitError{cause: err}
	}
	if streamID != "" && len(payload) > 0 {
		fs.downstreamStarted = true
	}
	return nil
}

func (fs *foldState) emitDone(streamID string) error {
	return fs.emitChunk(streamID, []byte("data: [DONE]\n\n"))
}

func (fs *foldState) sleepFor(delay time.Duration) {
	if delay <= 0 {
		return
	}
	if fs.sleep != nil {
		fs.sleep(delay)
		return
	}
	time.Sleep(delay)
}

func retryBackoff(cfg foldConfig, retryIndex int) time.Duration {
	delay := time.Duration(cfg.RetryInitialBackoffMS) * time.Millisecond
	maximum := time.Duration(cfg.RetryMaxBackoffMS) * time.Millisecond
	if delay <= 0 || maximum <= 0 {
		return 0
	}
	if delay >= maximum {
		return maximum
	}
	for i := 0; i < retryIndex; i++ {
		if delay >= maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

var transientHTTPStatusPattern = regexp.MustCompile(`(?i)(?:\bstatus(?:_code)?\b|\bcode\b|\bupstream\b|\bhttp\b)[^0-9]{0,16}(?:502|503|504)\b`)

func isTransientStartupError(err error) bool {
	if err == nil {
		return false
	}
	var emitErr *downstreamEmitError
	if errors.As(err, &emitErr) {
		return false
	}
	var upstreamErr *upstreamError
	if errors.As(err, &upstreamErr) {
		switch upstreamErr.status {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
	}

	message := strings.ToLower(err.Error())
	if transientHTTPStatusPattern.MatchString(message) {
		return true
	}
	for _, phrase := range []string{
		"connection refused",
		"econnrefused",
		"connection reset",
		"econnreset",
		"temporarily unavailable",
		"temporary unavailable",
		"service unavailable",
		"failed to validate api key",
		"bad gateway",
		"gateway timeout",
	} {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}

func terminalFailureError(terminal map[string]any) error {
	if terminal == nil || terminal["type"] != "response.failed" {
		return nil
	}

	response, _ := terminal["response"].(map[string]any)
	errorValue := terminal["error"]
	if response != nil && response["error"] != nil {
		errorValue = response["error"]
	}
	errorMap, _ := errorValue.(map[string]any)

	status := 0
	for _, value := range []any{
		valueFromMap(errorMap, "status_code"),
		valueFromMap(errorMap, "code"),
		valueFromMap(response, "status_code"),
		terminal["status_code"],
	} {
		if parsed, ok := statusCode(value); ok {
			status = parsed
			break
		}
	}

	detail := stringFromMap(errorMap, "message")
	if detail == "" {
		detail = stringFromMap(response, "message")
	}
	if detail == "" {
		detail = stringFromMap(terminal, "message")
	}
	if detail == "" && errorValue != nil {
		if raw, err := json.Marshal(errorValue); err == nil {
			detail = string(raw)
		}
	}
	if detail == "" {
		detail = "upstream response.failed"
	}
	return &upstreamError{status: status, msg: detail}
}

func valueFromMap(values map[string]any, key string) any {
	if values == nil {
		return nil
	}
	return values[key]
}

func stringFromMap(values map[string]any, key string) string {
	value, _ := valueFromMap(values, key).(string)
	return strings.TrimSpace(value)
}

func statusCode(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, typed > 0
	case int64:
		return int(typed), typed > 0
	case float64:
		return int(typed), typed > 0 && typed == float64(int(typed))
	case json.Number:
		parsed, err := strconv.Atoi(string(typed))
		return parsed, err == nil && parsed > 0
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		return parsed, err == nil && parsed > 0
	default:
		return 0, false
	}
}

const maxSSEBufferSize = 8 * 1024 * 1024

// CPA's stream_read returns payload chunks without trailing newlines, so we
// cannot rely on \n or \n\n to delimit SSE frames. We scan for "data:" prefixes
// and balance JSON braces to find event boundaries instead.
func (fs *foldState) processAndEmit(payload []byte, streamID string) (map[string]any, error) {
	fs.sseBuffer = append(fs.sseBuffer, payload...)
	if len(fs.sseBuffer) > maxSSEBufferSize {
		return nil, fmt.Errorf("sse buffer exceeded %d bytes", maxSSEBufferSize)
	}

	for {
		dataStart := findSubstring(fs.sseBuffer, []byte("data:"))
		if dataStart < 0 {
			break
		}
		jsonStart := dataStart + 5
		for jsonStart < len(fs.sseBuffer) && (fs.sseBuffer[jsonStart] == ' ' || fs.sseBuffer[jsonStart] == '\t') {
			jsonStart++
		}
		if jsonStart >= len(fs.sseBuffer) {
			break
		}

		if jsonStart+5 <= len(fs.sseBuffer) && string(fs.sseBuffer[jsonStart:jsonStart+5]) == "[DONE]" {
			fs.sseBuffer = fs.sseBuffer[jsonStart+5:]
			continue
		}

		if fs.sseBuffer[jsonStart] != '{' {
			fs.sseBuffer = fs.sseBuffer[dataStart+5:]
			continue
		}

		jsonEnd := findJSONEnd(fs.sseBuffer, jsonStart)
		if jsonEnd < 0 {
			break
		}

		dataBytes := fs.sseBuffer[jsonStart : jsonEnd+1]
		fs.sseBuffer = fs.sseBuffer[jsonEnd+1:]

		var ev map[string]any
		if err := json.Unmarshal(dataBytes, &ev); err != nil {
			return nil, fmt.Errorf("parse SSE data: %w", err)
		}

		term, err := fs.processEvent(ev, streamID)
		if err != nil {
			return nil, err
		}
		if term != nil {
			return term, nil
		}
	}

	return nil, nil
}

func findSubstring(data, sub []byte) int {
	if len(sub) == 0 || len(data) < len(sub) {
		return -1
	}
	for i := 0; i <= len(data)-len(sub); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			if data[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func findJSONEnd(data []byte, start int) int {
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(data); i++ {
		c := data[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func (fs *foldState) processEvent(ev map[string]any, streamID string) (map[string]any, error) {
	etype, _ := ev["type"].(string)

	if etype == "response.created" || etype == "response.in_progress" {
		if fs.roundNo == 1 {
			if etype == "response.created" {
				if r, ok := ev["response"].(map[string]any); ok {
					fs.baseResponse = r
				}
			}
			fs.stamp(ev)
			if err := fs.emitChunk(streamID, sseEvent(ev)); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}

	if terminalTypes[etype] {
		fs.terminal = ev
		if r, ok := ev["response"].(map[string]any); ok {
			if u, ok := r["usage"].(map[string]any); ok {
				fs.usage = cloneUsage(u)
			}
		}
		return ev, nil
	}

	oi := -1
	if v, ok := ev["output_index"].(float64); ok {
		oi = int(v)
	}

	if etype == "response.output_item.added" {
		item, _ := ev["item"].(map[string]any)
		if item == nil {
			item = map[string]any{}
		}
		itemType, _ := item["type"].(string)
		if itemType == "reasoning" {
			fs.kind[oi] = "reasoning"
			fs.oiToDS[oi] = fs.dsOI
			ev["output_index"] = fs.dsOI
			fs.dsOI++
			fs.stamp(ev)
			if err := fs.emitChunk(streamID, sseEvent(ev)); err != nil {
				return nil, err
			}
		} else {
			fs.kind[oi] = "buffered"
			fs.buffered = append(fs.buffered, bufferedEntry{oi: oi, item: item, events: []map[string]any{ev}})
		}
		return nil, nil
	}

	k := fs.kind[oi]
	if k == "reasoning" {
		if ds, ok := fs.oiToDS[oi]; ok {
			ev["output_index"] = ds
		}
		if etype == "response.output_item.done" {
			if item, ok := ev["item"].(map[string]any); ok {
				fs.roundReasoning = append(fs.roundReasoning, item)
				fs.finalOutput = append(fs.finalOutput, item)
			}
		}
		fs.stamp(ev)
		if err := fs.emitChunk(streamID, sseEvent(ev)); err != nil {
			return nil, err
		}
	} else if k == "buffered" {
		for i := range fs.buffered {
			if fs.buffered[i].oi == oi {
				fs.buffered[i].events = append(fs.buffered[i].events, ev)
				if etype == "response.output_item.done" {
					if item, ok := ev["item"].(map[string]any); ok {
						fs.buffered[i].item = item
					}
				}
				break
			}
		}
	} else {
		fs.stamp(ev)
		if err := fs.emitChunk(streamID, sseEvent(ev)); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (fs *foldState) stamp(ev map[string]any) {
	ev["sequence_number"] = fs.seq
	fs.seq++
}

func (fs *foldState) endRound(terminal map[string]any, usage map[string]any) {
	fs.terminal = terminal
	fs.usage = usage
	sumUsage(fs.summedUsage, usage)
	if fs.roundNo == 1 {
		fs.firstUsage = cloneUsage(usage)
	}

	rt := reasoningTokens(usage)
	n := tierN(rt)
	fs.roundsInfo = append(fs.roundsInfo, map[string]any{
		"round":            fs.roundNo,
		"reasoning_tokens": rt,
		"n":                n,
	})
	fs.debugf("round=%d completed reasoning_tokens=%s tier=%s", fs.roundNo, optionalIntString(rt), optionalIntString(n))
}

func (fs *foldState) shouldContinue() bool {
	if fs.terminal == nil {
		return false
	}
	etype, _ := fs.terminal["type"].(string)
	if etype != "response.completed" {
		return false
	}
	rt := reasoningTokens(fs.usage)
	n := tierN(rt)
	if !inContinueWindow(n, fs.config.MaxTierN) {
		return false
	}
	if !fs.hasEncryptedContent() {
		return false
	}
	return fs.roundNo <= fs.config.MaxContinue
}

func (fs *foldState) stoppedReason() string {
	etype, _ := fs.terminal["type"].(string)
	if etype != "response.completed" {
		return ""
	}
	rt := reasoningTokens(fs.usage)
	n := tierN(rt)
	if n == nil {
		return ""
	}
	if !fs.hasEncryptedContent() {
		return "no_encrypted_content"
	}
	if fs.roundNo > fs.config.MaxContinue {
		return "max_continue"
	}
	return "tier_out_of_window"
}

func (fs *foldState) hasEncryptedContent() bool {
	if len(fs.roundReasoning) == 0 {
		return false
	}
	last := fs.roundReasoning[len(fs.roundReasoning)-1]
	s, ok := last["encrypted_content"].(string)
	return ok && s != ""
}

func (fs *foldState) prepareNextRound() {
	tail := make([]any, 0, len(fs.roundReasoning)+1)
	for _, r := range fs.roundReasoning {
		tail = append(tail, r)
	}
	tail = append(tail, commentaryNudge(fs.config.MarkerText))
	fs.replayTail = append(fs.replayTail, tail...)
}

func (fs *foldState) debugf(format string, args ...any) {
	if !fs.config.DebugLog {
		return
	}
	pluginLog("debug", fmt.Sprintf(format, args...))
}

func optionalIntString(value *int) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%d", *value)
}

func (fs *foldState) flushCleanStop(streamID string) error {
	for _, entry := range fs.buffered {
		for _, ev := range entry.events {
			if _, ok := ev["output_index"]; ok {
				ev["output_index"] = fs.dsOI
			}
			fs.stamp(ev)
			if err := fs.emitChunk(streamID, sseEvent(ev)); err != nil {
				return err
			}
		}
		fs.dsOI++
		fs.finalOutput = append(fs.finalOutput, entry.item)
	}
	return nil
}

func (fs *foldState) terminalEvent() map[string]any {
	return terminalEvent(
		fs.terminal,
		fs.baseResponse,
		fs.finalOutput,
		agentUsage(fs.firstUsage, fs.summedUsage, fs.usage, true),
		fs.roundsInfo,
		fs.summedUsage,
		fs.stoppedReason(),
		"",
	)
}

func (fs *foldState) incompleteEvent(reason string) map[string]any {
	return terminalEvent(
		nil,
		fs.baseResponse,
		fs.finalOutput,
		agentUsage(fs.firstUsage, fs.summedUsage, fs.usage, false),
		fs.roundsInfo,
		fs.summedUsage,
		reason,
		reason,
	)
}
