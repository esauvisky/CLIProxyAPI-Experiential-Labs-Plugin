package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcExecutorHTTPRequest struct {
	pluginapi.ExecutorHTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcHostHTTPStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

type rpcHostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func handleExecutorExecute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	resp, errExec := executeNonStream(context.Background(), req)
	if errExec != nil {
		return nil, errExec
	}
	return okEnvelope(resp)
}

func handleExecutorExecuteStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return nil, fmt.Errorf("stream_id is required for executor.execute_stream")
	}

	upstreamStream, prepared, errStart := openUpstreamStream(req)
	if errStart != nil {
		return nil, errStart
	}

	go forwardUpstreamStream(prepared, upstreamStream.StreamID, streamID)

	return okEnvelope(map[string]any{
		"headers": http.Header{
			"Content-Type": []string{"text/event-stream; charset=utf-8"},
		},
	})
}

func handleExecutorCountTokens(_ []byte) ([]byte, error) {
	return nil, statusError{
		code:       "unsupported",
		message:    "Experiential Labs executor does not implement count_tokens",
		httpStatus: http.StatusNotImplemented,
	}
}

func handleExecutorHTTPRequest(raw []byte) ([]byte, error) {
	var req rpcExecutorHTTPRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	apiKey, errKey := apiKeyFromStorage(req.StorageJSON)
	if errKey != nil {
		return nil, errKey
	}
	headers := cloneHTTPHeader(req.Headers)
	if headers == nil {
		headers = make(http.Header)
	}
	if headers.Get("Authorization") == "" {
		headers.Set("Authorization", "Bearer "+apiKey)
	}
	resp, errHTTP := doHostHTTP(req.HostCallbackID, req.Method, req.URL, headers, req.Body)
	if errHTTP != nil {
		return nil, errHTTP
	}
	return okEnvelope(pluginapi.ExecutorHTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Headers,
		Body:       resp.Body,
	})
}

type preparedUpstreamCall struct {
	baseModel       string
	clientFormat    sdktranslator.Format
	wireFormat      sdktranslator.Format
	originalRequest []byte
	translatedReq   []byte
	upstreamURL     string
	upstreamHeaders http.Header
}

func prepareUpstreamRequest(req rpcExecutorRequest, stream bool) (preparedUpstreamCall, error) {
	model := strings.TrimSpace(req.Model)
	if strings.LastIndex(model, "(") >= 0 && strings.HasSuffix(model, ")") {
		return preparedUpstreamCall{}, statusError{
			code:       "unsupported_thinking_suffix",
			message:    fmt.Sprintf("thinking suffix on model %q is not supported: the Experiential Labs docs do not document a reasoning request parameter", model),
			httpStatus: http.StatusBadRequest,
		}
	}
	spec, errSpec := lookupConfirmedModel(model)
	if errSpec != nil {
		return preparedUpstreamCall{}, errSpec
	}

	clientFormatStr := firstNonEmpty(req.Format, req.SourceFormat)
	if clientFormatStr == "" {
		return preparedUpstreamCall{}, fmt.Errorf("executor request has neither format nor source_format")
	}
	clientFormat, errClient := normalizeTranslatorFormat(clientFormatStr)
	if errClient != nil {
		return preparedUpstreamCall{}, errClient
	}
	wireFormat, errWire := normalizeTranslatorFormat(string(spec.Protocol))
	if errWire != nil {
		return preparedUpstreamCall{}, errWire
	}

	if len(req.Payload) == 0 {
		return preparedUpstreamCall{}, fmt.Errorf("executor request has no payload")
	}
	originalRequest := req.OriginalRequest
	if len(originalRequest) == 0 {
		originalRequest = req.Payload
	}

	translated := bytes.Clone(req.Payload)
	if clientFormat != wireFormat {
		env := sdktranslator.TranslateRequestEnvelope(context.Background(), clientFormat, wireFormat, sdktranslator.RequestEnvelope{
			Format: clientFormat,
			Model:  model,
			Stream: stream,
			Body:   req.Payload,
		})
		if env.Err != nil {
			return preparedUpstreamCall{}, env.Err
		}
		translated = env.Body
	}

	translated, errFinal := finalizeWirePayload(translated, spec.Protocol, model, stream)
	if errFinal != nil {
		return preparedUpstreamCall{}, errFinal
	}

	apiKey, errKey := apiKeyFromStorage(req.StorageJSON)
	if errKey != nil {
		return preparedUpstreamCall{}, errKey
	}

	cfg := currentConfig()
	upstreamURL, errURL := buildUpstreamEndpointURL(cfg.BaseURL, spec.Protocol)
	if errURL != nil {
		return preparedUpstreamCall{}, errURL
	}

	headers := http.Header{
		"Authorization": []string{"Bearer " + apiKey},
		"Content-Type":  []string{"application/json"},
	}
	if stream {
		headers.Set("Accept", "text/event-stream")
	} else {
		headers.Set("Accept", "application/json")
	}
	if spec.Protocol == wireProtocolAnthropic {
		headers.Set("anthropic-version", "2023-06-01")
	}

	return preparedUpstreamCall{
		baseModel:       model,
		clientFormat:    clientFormat,
		wireFormat:      wireFormat,
		originalRequest: originalRequest,
		translatedReq:   translated,
		upstreamURL:     upstreamURL,
		upstreamHeaders: headers,
	}, nil
}

func finalizeWirePayload(payload []byte, proto wireProtocol, model string, stream bool) ([]byte, error) {
	out := bytes.Clone(payload)
	var errEdit error
	switch proto {
	case wireProtocolChatCompletions:
		if out, errEdit = sjson.SetBytes(out, "model", model); errEdit != nil {
			return nil, errEdit
		}
		if out, errEdit = sjson.SetBytes(out, "stream", stream); errEdit != nil {
			return nil, errEdit
		}
		if stream {
			out, errEdit = sjson.SetBytes(out, "stream_options.include_usage", true)
		} else {
			out, errEdit = sjson.DeleteBytes(out, "stream_options")
		}
		if errEdit != nil {
			return nil, errEdit
		}
	case wireProtocolResponses:
		if out, errEdit = sjson.SetBytes(out, "model", model); errEdit != nil {
			return nil, errEdit
		}
		if out, errEdit = sjson.SetBytes(out, "stream", stream); errEdit != nil {
			return nil, errEdit
		}
	case wireProtocolAnthropic:
		if out, errEdit = sjson.SetBytes(out, "model", model); errEdit != nil {
			return nil, errEdit
		}
		if out, errEdit = sjson.SetBytes(out, "stream", stream); errEdit != nil {
			return nil, errEdit
		}
		if !gjson.GetBytes(out, "max_tokens").Exists() {
			return nil, fmt.Errorf("Anthropic Messages request for model %s requires max_tokens", model)
		}
	default:
		return nil, fmt.Errorf("unsupported wire protocol %q", proto)
	}
	return out, nil
}

func buildUpstreamEndpointURL(baseURL string, proto wireProtocol) (string, error) {
	switch proto {
	case wireProtocolChatCompletions:
		return baseURL + "/v1/chat/completions", nil
	case wireProtocolResponses:
		return baseURL + "/v1/responses", nil
	case wireProtocolAnthropic:
		return baseURL + "/v1/messages", nil
	default:
		return "", fmt.Errorf("unsupported wire protocol %q", proto)
	}
}

func executeNonStream(ctx context.Context, req rpcExecutorRequest) (pluginapi.ExecutorResponse, error) {
	prepared, errPrep := prepareUpstreamRequest(req, false)
	if errPrep != nil {
		return pluginapi.ExecutorResponse{}, errPrep
	}
	httpResp, errHTTP := doHostHTTP(req.HostCallbackID, http.MethodPost, prepared.upstreamURL, prepared.upstreamHeaders, prepared.translatedReq)
	if errHTTP != nil {
		return pluginapi.ExecutorResponse{}, errHTTP
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return pluginapi.ExecutorResponse{}, statusError{
			code:       "upstream_error",
			message:    extractUpstreamErrorMessage(httpResp.StatusCode, httpResp.Body),
			httpStatus: httpResp.StatusCode,
		}
	}

	outPayload := bytes.Clone(httpResp.Body)
	if prepared.wireFormat != prepared.clientFormat {
		var param any
		outPayload = sdktranslator.TranslateNonStream(
			ctx,
			prepared.wireFormat,
			prepared.clientFormat,
			prepared.baseModel,
			prepared.originalRequest,
			prepared.translatedReq,
			httpResp.Body,
			&param,
		)
	}
	return pluginapi.ExecutorResponse{
		Payload: outPayload,
		Headers: httpResp.Headers,
	}, nil
}

func openUpstreamStream(req rpcExecutorRequest) (rpcHostHTTPStreamResponse, preparedUpstreamCall, error) {
	prepared, errPrep := prepareUpstreamRequest(req, true)
	if errPrep != nil {
		return rpcHostHTTPStreamResponse{}, preparedUpstreamCall{}, errPrep
	}
	payload := map[string]any{
		"host_callback_id": req.HostCallbackID,
		"method":           http.MethodPost,
		"url":              prepared.upstreamURL,
		"headers":          prepared.upstreamHeaders,
		"body":             prepared.translatedReq,
	}
	raw, errCall := callHost(pluginabi.MethodHostHTTPDoStream, payload)
	if errCall != nil {
		return rpcHostHTTPStreamResponse{}, preparedUpstreamCall{}, errCall
	}
	var streamResp rpcHostHTTPStreamResponse
	if errUnmarshal := json.Unmarshal(raw, &streamResp); errUnmarshal != nil {
		return rpcHostHTTPStreamResponse{}, preparedUpstreamCall{}, errUnmarshal
	}
	if streamResp.StatusCode < 200 || streamResp.StatusCode >= 300 {
		errBody, errRead := readAndCloseErrorStream(streamResp.StreamID)
		if errRead != nil {
			return rpcHostHTTPStreamResponse{}, preparedUpstreamCall{}, fmt.Errorf("Experiential Labs upstream HTTP %d and error body unreadable: %w", streamResp.StatusCode, errRead)
		}
		return rpcHostHTTPStreamResponse{}, preparedUpstreamCall{}, statusError{
			code:       "upstream_error",
			message:    extractUpstreamErrorMessage(streamResp.StatusCode, errBody),
			httpStatus: streamResp.StatusCode,
		}
	}
	if strings.TrimSpace(streamResp.StreamID) == "" {
		return rpcHostHTTPStreamResponse{}, preparedUpstreamCall{}, fmt.Errorf("upstream stream opened with empty stream_id")
	}
	return streamResp, prepared, nil
}

func forwardUpstreamStream(prepared preparedUpstreamCall, httpStreamID, pluginStreamID string) {
	defer closeHostHTTPStream(httpStreamID)

	ctx := context.Background()
	needsTranslation := prepared.wireFormat != prepared.clientFormat
	var param any
	var pending []byte

	emitChunk := func(chunk []byte) error {
		if len(chunk) == 0 {
			return nil
		}
		_, errEmit := callHost(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{
			StreamID: pluginStreamID,
			Payload:  chunk,
		})
		return errEmit
	}

	processLine := func(line []byte) error {
		trimmed := bytes.TrimRight(line, "\r\n")
		if !needsTranslation {
			return emitChunk(line)
		}
		if len(bytes.TrimSpace(trimmed)) == 0 {
			return nil
		}
		frames := sdktranslator.TranslateStream(
			ctx,
			prepared.wireFormat,
			prepared.clientFormat,
			prepared.baseModel,
			prepared.originalRequest,
			prepared.translatedReq,
			trimmed,
			&param,
		)
		for _, frame := range frames {
			if len(frame) == 0 {
				continue
			}
			if !bytes.HasSuffix(frame, []byte("\n\n")) {
				if bytes.HasSuffix(frame, []byte("\n")) {
					frame = append(frame, '\n')
				} else {
					frame = append(frame, '\n', '\n')
				}
			}
			if errEmit := emitChunk(frame); errEmit != nil {
				return errEmit
			}
		}
		return nil
	}

	var streamErr string
	for {
		rawRead, errCall := callHost(pluginabi.MethodHostHTTPStreamRead, map[string]any{
			"stream_id": httpStreamID,
		})
		if errCall != nil {
			streamErr = errCall.Error()
			break
		}
		var readResp rpcHostHTTPStreamReadResponse
		if errUnmarshal := json.Unmarshal(rawRead, &readResp); errUnmarshal != nil {
			streamErr = errUnmarshal.Error()
			break
		}
		if len(readResp.Payload) > 0 {
			pending = append(pending, readResp.Payload...)
			for {
				idx := bytes.IndexByte(pending, '\n')
				if idx < 0 {
					break
				}
				line := append([]byte(nil), pending[:idx+1]...)
				pending = pending[idx+1:]
				if errLine := processLine(line); errLine != nil {
					streamErr = errLine.Error()
					break
				}
			}
			if streamErr != "" {
				break
			}
		}
		if readResp.Error != "" {
			streamErr = readResp.Error
			break
		}
		if readResp.Done {
			break
		}
	}

	if streamErr == "" && len(pending) > 0 {
		if errLine := processLine(pending); errLine != nil {
			streamErr = errLine.Error()
		}
	}
	if streamErr == "" && needsTranslation && prepared.wireFormat == sdktranslator.FormatOpenAI {
		// Flush terminal [DONE] for translators that finalize on data: [DONE]
		frames := sdktranslator.TranslateStream(
			ctx,
			prepared.wireFormat,
			prepared.clientFormat,
			prepared.baseModel,
			prepared.originalRequest,
			prepared.translatedReq,
			[]byte("data: [DONE]"),
			&param,
		)
		for _, frame := range frames {
			if len(frame) == 0 {
				continue
			}
			if !bytes.HasSuffix(frame, []byte("\n\n")) {
				frame = append(frame, '\n', '\n')
			}
			if errEmit := emitChunk(frame); errEmit != nil {
				streamErr = errEmit.Error()
				break
			}
		}
	}

	_, _ = callHost(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{
		StreamID: pluginStreamID,
		Error:    streamErr,
	})
}

func readAndCloseErrorStream(httpStreamID string) ([]byte, error) {
	if strings.TrimSpace(httpStreamID) == "" {
		return nil, fmt.Errorf("upstream error stream has empty stream_id")
	}
	defer closeHostHTTPStream(httpStreamID)
	var out []byte
	for i := 0; i < 8; i++ {
		raw, errCall := callHost(pluginabi.MethodHostHTTPStreamRead, map[string]any{
			"stream_id": httpStreamID,
		})
		if errCall != nil {
			return nil, errCall
		}
		var resp rpcHostHTTPStreamReadResponse
		if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		if resp.Error != "" {
			return nil, fmt.Errorf("read upstream error stream: %s", resp.Error)
		}
		out = append(out, resp.Payload...)
		if resp.Done || len(out) > 16384 {
			break
		}
	}
	return out, nil
}

func closeHostHTTPStream(httpStreamID string) {
	if strings.TrimSpace(httpStreamID) == "" {
		return
	}
	_, _ = callHost(pluginabi.MethodHostHTTPStreamClose, map[string]any{
		"stream_id": httpStreamID,
	})
}

func extractUpstreamErrorMessage(status int, body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if msg := gjson.GetBytes(body, "error.message").String(); strings.TrimSpace(msg) != "" {
		return strings.TrimSpace(msg)
	}
	if msg := gjson.GetBytes(body, "message").String(); strings.TrimSpace(msg) != "" {
		return strings.TrimSpace(msg)
	}
	if trimmed != "" {
		return trimmed
	}
	return fmt.Sprintf("Experiential Labs upstream returned HTTP %d", status)
}

func normalizeTranslatorFormat(format string) (sdktranslator.Format, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "openai":
		return sdktranslator.FormatOpenAI, nil
	case "claude":
		return sdktranslator.FormatClaude, nil
	case "openai-response":
		return sdktranslator.FormatOpenAIResponse, nil
	default:
		return "", fmt.Errorf("unsupported request format %q", format)
	}
}

func cloneHTTPHeader(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := make(http.Header, len(h))
	for k, vals := range h {
		out[k] = append([]string(nil), vals...)
	}
	return out
}
