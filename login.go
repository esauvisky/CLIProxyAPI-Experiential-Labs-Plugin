package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type managementRegistration struct {
	Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
	Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
}

type loginSubmitRequest struct {
	State  string `json:"state"`
	APIKey string `json:"api_key"`
}

func managementRegistrationPayload() managementRegistration {
	return managementRegistration{
		Routes: []pluginapi.ManagementRoute{
			{
				Method:      http.MethodPost,
				Path:        loginSubmitPath,
				Description: "Stores the Experiential Labs API key for a pending login state.",
			},
		},
		Resources: []pluginapi.ResourceRoute{
			{
				Path:        loginPagePath,
				Description: "Enter the Experiential Labs API key to finish login.",
			},
		},
	}
}

func handleResponseInterceptAfter(raw []byte) ([]byte, error) {
	var req pluginapi.ResponseInterceptRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}
	observeOriginFromHeaders(req.RequestHeaders)
	return okEnvelope(pluginapi.ResponseInterceptResponse{})
}

func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	observeOriginFromHeaders(req.Headers)
	cleanedPath := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	if strings.HasSuffix(cleanedPath, "/plugins/"+pluginID+"/login") {
		if strings.EqualFold(req.Method, http.MethodPost) {
			return handleLoginSubmit(req.Body)
		}
		return nil, fmt.Errorf("unsupported method %s for %s", req.Method, req.Path)
	}

	// /v0/resource/plugins/experiential-labs/login serves only the static UI asset.
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":  []string{"text/html; charset=utf-8"},
			"Cache-Control": []string{"no-store"},
		},
		Body: []byte(loginPageHTML),
	})
}

func handleLoginSubmit(body []byte) ([]byte, error) {
	var req loginSubmitRequest
	if errUnmarshal := json.Unmarshal(body, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	stateKey := strings.TrimSpace(req.State)
	pending, errPending := loadPendingLogin(stateKey)
	if errPending != nil {
		return nil, errPending
	}
	apiKey, errKey := credentialAPIKey(storedCredential{APIKey: strings.TrimSpace(req.APIKey)})
	if errKey != nil {
		return nil, errKey
	}
	pending.apiKey = apiKey
	pendingLoginStates.Store(stateKey, pending)
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type": []string{"application/json; charset=utf-8"},
		},
		Body: []byte(`{"status":"ok"}`),
	})
}

const loginPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Experiential Labs login</title>
<style>
body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: #1d1b18; color: #f6f4f1; font-family: system-ui, sans-serif; }
main { width: min(420px, 92vw); display: grid; gap: 14px; }
h1 { margin: 0; font-size: 20px; font-weight: 600; }
label { display: grid; gap: 6px; font-size: 13px; color: #c9c3bb; }
input { font: inherit; padding: 9px 10px; border-radius: 8px; border: 1px solid #4a453f; background: #151412; color: #f6f4f1; }
button { font: inherit; padding: 9px 12px; border-radius: 8px; border: 0; background: #8b8680; color: #ffffff; cursor: pointer; }
button:disabled { opacity: 0.6; cursor: default; }
#status { min-height: 1.2em; margin: 0; font-size: 13px; color: #c9c3bb; white-space: pre-wrap; }
#status.error { color: #f1b0a6; }
#status.ok { color: #6ee7b7; }
</style>
</head>
<body>
<main>
<h1>Experiential Labs login</h1>
<form id="login">
<label id="mgmtWrap" style="display:none;">Management key <input type="password" id="mgmtKey" autocomplete="off"></label>
<label>Experiential Labs API key <input type="password" id="apiKey" placeholder="xpl_..." autocomplete="off" required></label>
<button type="submit" id="submit">Submit</button>
</form>
<p id="status"></p>
</main>
<script>
const form = document.getElementById('login');
const statusEl = document.getElementById('status');
const submitBtn = document.getElementById('submit');
const mgmtWrap = document.getElementById('mgmtWrap');
const mgmtInput = document.getElementById('mgmtKey');

function deobfuscateCPAStore(payload) {
  const prefix = 'enc::v2::';
  if (!payload.startsWith(prefix)) {
    throw new Error('cli-proxy-auth is not in the enc::v2:: format this page understands');
  }
  const bin = atob(payload.slice(prefix.length));
  const keyBytes = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + window.location.host);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i) ^ keyBytes[i % keyBytes.length];
  return new TextDecoder().decode(out);
}

function detectStoredManagementKey() {
  const authRaw = localStorage.getItem('cli-proxy-auth');
  if (authRaw === null) return '';
  const k = JSON.parse(deobfuscateCPAStore(authRaw)).state.managementKey;
  return typeof k === 'string' ? k.trim() : '';
}

const initialKey = detectStoredManagementKey();
if (initialKey) {
  mgmtInput.value = initialKey;
} else {
  mgmtWrap.style.display = 'grid';
}

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  const state = new URLSearchParams(window.location.search).get('state') || '';
  const mgmtKey = (mgmtInput.value || detectStoredManagementKey()).trim();
  const apiKey = document.getElementById('apiKey').value.trim();
  submitBtn.disabled = true;
  statusEl.className = '';
  statusEl.textContent = 'Submitting...';
  const response = await fetch('/v0/management/plugins/experiential-labs/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'Authorization': 'Bearer ' + mgmtKey, 'X-Management-Key': mgmtKey },
    body: JSON.stringify({ state: state, api_key: apiKey }),
  });
  const text = await response.text();
  if (response.status === 401 || response.status === 403) {
    mgmtWrap.style.display = 'grid';
  }
  if (!response.ok) {
    statusEl.className = 'error';
    statusEl.textContent = 'Failed (HTTP ' + response.status + '): ' + text;
    submitBtn.disabled = false;
    return;
  }
  statusEl.className = 'ok';
  statusEl.textContent = 'API key received. Return to CLIProxyAPI to finish the login.';
});
</script>
</body>
</html>
`
