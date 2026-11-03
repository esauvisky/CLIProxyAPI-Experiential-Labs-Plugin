package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var apiKeyPattern = regexp.MustCompile(`^xpl_[0-9a-fA-F]{32,}$`)

const (
	loginStateTTL   = 15 * time.Minute
	loginPagePath   = "/v0/resource/plugins/" + pluginID + "/login"
	loginSubmitPath = "/v0/management/plugins/" + pluginID + "/login"
)

var pendingLoginStates sync.Map

type pendingLogin struct {
	expiresAt time.Time
	apiKey    string
}

type rpcAuthLoginStartRequest struct {
	pluginapi.AuthLoginStartRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcAuthLoginPollRequest struct {
	pluginapi.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type storedCredential struct {
	APIKey string `json:"api_key,omitempty"`
}

func handleAuthParse(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cred, okParse := parseStoredCredential(req.RawJSON)
	if !okParse {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	authData, errBuild := buildAuthDataFromCredential(cred, req.FileName)
	if errBuild != nil {
		return nil, errBuild
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth:    authData,
	})
}

// parseStoredCredential reports whether the payload is an Experiential Labs credential.
// Validation of the api_key itself happens in credentialAPIKey.
func parseStoredCredential(rawJSON []byte) (storedCredential, bool) {
	trimmed := bytes.TrimSpace(rawJSON)
	if len(trimmed) == 0 {
		return storedCredential{}, false
	}
	var raw map[string]any
	if errUnmarshal := json.Unmarshal(trimmed, &raw); errUnmarshal != nil || raw == nil {
		return storedCredential{}, false
	}
	if strings.ToLower(stringFromMap(raw, "type")) != providerID {
		return storedCredential{}, false
	}
	return storedCredential{
		APIKey: stringFromMap(raw, "api_key"),
	}, true
}

func credentialAPIKey(cred storedCredential) (string, error) {
	if !apiKeyPattern.MatchString(cred.APIKey) {
		return "", statusError{
			code:       "invalid_credential",
			message:    "Experiential Labs credential requires api_key of the form xpl_ followed by at least 32 hex characters",
			httpStatus: http.StatusUnauthorized,
		}
	}
	return cred.APIKey, nil
}

func apiKeyFromStorage(storageJSON []byte) (string, error) {
	cred, okParse := parseStoredCredential(storageJSON)
	if !okParse {
		return "", statusError{
			code:       "unauthorized",
			message:    "auth storage is not an Experiential Labs credential",
			httpStatus: http.StatusUnauthorized,
		}
	}
	return credentialAPIKey(cred)
}

func buildAuthDataFromCredential(cred storedCredential, fileName string) (pluginapi.AuthData, error) {
	key, errKey := credentialAPIKey(cred)
	if errKey != nil {
		return pluginapi.AuthData{}, errKey
	}
	sum := sha256.Sum256([]byte(key))
	fingerprint := hex.EncodeToString(sum[:])[:12]
	if strings.TrimSpace(fileName) == "" {
		fileName = fmt.Sprintf("experiential-labs-%s.json", fingerprint)
	}
	id := strings.TrimSpace(fileName)

	storageBytes, errStorage := marshalJSONNoEscape(map[string]any{
		"type":    providerID,
		"api_key": key,
	})
	if errStorage != nil {
		return pluginapi.AuthData{}, errStorage
	}

	return pluginapi.AuthData{
		Provider:    providerID,
		ID:          id,
		FileName:    fileName,
		Label:       "Experiential Labs",
		StorageJSON: storageBytes,
		Metadata: map[string]any{
			"type":            providerID,
			"key_fingerprint": fingerprint,
		},
		Attributes: map[string]string{
			"api_key":  key,
			"base_url": currentConfig().BaseURL,
		},
	}, nil
}

func handleAuthLoginStart(raw []byte) ([]byte, error) {
	var req rpcAuthLoginStartRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	base, errBase := url.Parse(strings.TrimSpace(req.BaseURL))
	if errBase != nil {
		return nil, fmt.Errorf("parse auth.login.start BaseURL %q: %w", req.BaseURL, errBase)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("auth.login.start BaseURL %q has no scheme or host", req.BaseURL)
	}

	stateRaw := make([]byte, 32)
	if _, errRand := rand.Read(stateRaw); errRand != nil {
		return nil, fmt.Errorf("generate Experiential Labs login state: %w", errRand)
	}
	stateKey := hex.EncodeToString(stateRaw)
	expiresAt := time.Now().UTC().Add(loginStateTTL)
	pendingLoginStates.Store(stateKey, pendingLogin{expiresAt: expiresAt})

	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerID,
		URL:       base.Scheme + "://" + base.Host + loginPagePath + "?state=" + stateKey,
		State:     stateKey,
		ExpiresAt: expiresAt,
	})
}

func loadPendingLogin(stateKey string) (pendingLogin, error) {
	loaded, ok := pendingLoginStates.Load(stateKey)
	if !ok {
		return pendingLogin{}, statusError{
			code:       "invalid_login_state",
			message:    "unknown Experiential Labs login state",
			httpStatus: http.StatusBadRequest,
		}
	}
	pending := loaded.(pendingLogin)
	if time.Now().UTC().After(pending.expiresAt) {
		pendingLoginStates.Delete(stateKey)
		return pendingLogin{}, statusError{
			code:       "expired_login_state",
			message:    "Experiential Labs login state expired",
			httpStatus: http.StatusBadRequest,
		}
	}
	return pending, nil
}

func handleAuthLoginPoll(raw []byte) ([]byte, error) {
	var req rpcAuthLoginPollRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	stateKey := strings.TrimSpace(req.State)
	pending, errPending := loadPendingLogin(stateKey)
	if errPending != nil {
		return nil, errPending
	}
	if pending.apiKey == "" {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "Waiting for the Experiential Labs API key to be submitted",
		})
	}
	authData, errBuild := buildAuthDataFromCredential(storedCredential{APIKey: pending.apiKey}, "")
	if errBuild != nil {
		return nil, errBuild
	}
	pendingLoginStates.Delete(stateKey)
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "Experiential Labs login complete",
		Auth:    authData,
	})
}

func doHostHTTP(hostCallbackID, method, targetURL string, headers http.Header, body []byte) (pluginapi.HTTPResponse, error) {
	payload := map[string]any{
		"host_callback_id": hostCallbackID,
		"method":           method,
		"url":              targetURL,
		"headers":          headers,
		"body":             body,
	}
	raw, errCall := callHost(pluginabi.MethodHostHTTPDo, payload)
	if errCall != nil {
		return pluginapi.HTTPResponse{}, errCall
	}
	var resp pluginapi.HTTPResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return pluginapi.HTTPResponse{}, errUnmarshal
	}
	return resp, nil
}

func stringFromMap(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, okStr := v.(string); okStr && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}
