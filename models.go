package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type wireProtocol string

const (
	wireProtocolChatCompletions wireProtocol = "openai"
	wireProtocolResponses       wireProtocol = "openai-response"
	wireProtocolAnthropic       wireProtocol = "claude"
)

type rpcAuthModelRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type modelSpec struct {
	DisplayName     string
	Protocol        wireProtocol
	ContextLen      int64
	MaxOutput       int64
	InputModalities []string
}

// confirmedModels holds only models whose wire protocol is stated on
// platform.experientiallabs.ai/models/<slug>. Catalog IDs from /v1/models that
// are absent here are rejected at execution time instead of guessing a protocol.
var confirmedModels = map[string]modelSpec{
	"gpt-6-astra": {
		DisplayName:     "GPT-6 Astra",
		Protocol:        wireProtocolChatCompletions,
		ContextLen:      1050000,
		MaxOutput:       128000,
		InputModalities: []string{"text", "image"},
	},
	"glm-5.3": {
		DisplayName:     "GLM 5.3",
		Protocol:        wireProtocolChatCompletions,
		ContextLen:      1050000,
		MaxOutput:       131000,
		InputModalities: []string{"text"},
	},
}

func lookupConfirmedModel(modelID string) (modelSpec, error) {
	spec, ok := confirmedModels[modelID]
	if !ok {
		return modelSpec{}, statusError{
			code:       "unconfirmed_protocol",
			message:    fmt.Sprintf("model %q has no wire protocol confirmed in the Experiential Labs docs", modelID),
			httpStatus: http.StatusBadRequest,
		}
	}
	return spec, nil
}

func handleModelForAuth(raw []byte) ([]byte, error) {
	var req rpcAuthModelRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	apiKey, errKey := apiKeyFromStorage(req.StorageJSON)
	if errKey != nil {
		return nil, errKey
	}
	ids, errIDs := fetchCatalogModelIDs(req.HostCallbackID, apiKey)
	if errIDs != nil {
		return nil, errIDs
	}
	created := time.Now().Unix()
	models := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		models = append(models, modelInfoFor(id, created))
	}
	return okEnvelope(pluginapi.ModelResponse{
		Provider: providerID,
		Models:   models,
	})
}

func fetchCatalogModelIDs(hostCallbackID, apiKey string) ([]string, error) {
	cfg := currentConfig()
	resp, errHTTP := doHostHTTP(hostCallbackID, http.MethodGet, cfg.BaseURL+"/v1/models", http.Header{
		"Authorization": []string{"Bearer " + apiKey},
		"Accept":        []string{"application/json"},
	}, nil)
	if errHTTP != nil {
		return nil, fmt.Errorf("request Experiential Labs model list: %w", errHTTP)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError{
			code:       "catalog_error",
			message:    fmt.Sprintf("Experiential Labs model list HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(resp.Body))),
			httpStatus: resp.StatusCode,
		}
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(resp.Body, &parsed); errUnmarshal != nil {
		return nil, fmt.Errorf("decode Experiential Labs model list: %w", errUnmarshal)
	}
	ids := make([]string, 0, len(parsed.Data))
	for _, item := range parsed.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			return nil, fmt.Errorf("Experiential Labs model list contains an entry without id")
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func modelInfoFor(id string, created int64) pluginapi.ModelInfo {
	info := pluginapi.ModelInfo{
		ID:          id,
		Object:      "model",
		Created:     created,
		OwnedBy:     providerID,
		DisplayName: id,
		Name:        id,
		Version:     id,
		UserDefined: true,
	}
	if spec, ok := confirmedModels[id]; ok {
		info.Type = string(spec.Protocol)
		info.DisplayName = spec.DisplayName
		info.ContextLength = spec.ContextLen
		info.InputTokenLimit = spec.ContextLen
		info.MaxCompletionTokens = spec.MaxOutput
		info.OutputTokenLimit = spec.MaxOutput
		info.SupportedInputModalities = spec.InputModalities
		info.SupportedOutputModalities = []string{"text"}
	}
	return info
}
