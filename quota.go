package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type rpcQuotaFetchRequest struct {
	pluginapi.QuotaFetchRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func handleQuotaDescribe(_ []byte) ([]byte, error) {
	return okEnvelope(pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{providerID},
		DisplayName:        "Experiential Labs Credits",
		SupportsReset:      false,
	})
}

// handleQuotaFetch reads the account credit balance from GET /api/v1/credits.
// The balance is org-wide, so every credential of the same org reports the same value.
func handleQuotaFetch(raw []byte) ([]byte, error) {
	var req rpcQuotaFetchRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	apiKey, errKey := apiKeyFromStorage(req.StorageJSON)
	if errKey != nil {
		return nil, errKey
	}
	cfg := currentConfig()
	resp, errHTTP := doHostHTTP(req.HostCallbackID, http.MethodGet, cfg.BaseURL+"/api/v1/credits", http.Header{
		"Authorization": []string{"Bearer " + apiKey},
		"Accept":        []string{"application/json"},
	}, nil)
	if errHTTP != nil {
		return nil, fmt.Errorf("request Experiential Labs credits: %w", errHTTP)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError{
			code:       "quota_error",
			message:    fmt.Sprintf("Experiential Labs credits HTTP %d: %s", resp.StatusCode, string(resp.Body)),
			httpStatus: resp.StatusCode,
		}
	}
	var parsed struct {
		Data struct {
			TotalCredits float64 `json:"total_credits"`
			TotalUsage   float64 `json:"total_usage"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(resp.Body, &parsed); errUnmarshal != nil {
		return nil, fmt.Errorf("decode Experiential Labs credits: %w", errUnmarshal)
	}

	total := parsed.Data.TotalCredits
	remaining := total - parsed.Data.TotalUsage
	if remaining < 0 {
		remaining = 0
	}
	fraction := 0.0
	if total > 0 {
		fraction = remaining / total
		if fraction > 1 {
			fraction = 1
		}
	}

	return okEnvelope(pluginapi.QuotaFetchResponse{
		Groups: []pluginapi.QuotaGroup{
			{
				DisplayName: "Experiential Labs Credits",
				Buckets: []pluginapi.QuotaBucket{
					{
						Window:            "balance",
						RemainingFraction: fraction,
						Description:       fmt.Sprintf("$%.4f of $%.4f credits remaining", remaining, total),
					},
				},
			},
		},
	})
}
