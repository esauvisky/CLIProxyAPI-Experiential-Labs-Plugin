# CLIProxyAPI Experiential Labs Plugin

This ABI v1 plugin adds the **Experiential Labs gateway** (`experiential-labs`, https://api.experientiallabs.ai) to CLIProxyAPI as an API-key provider. There is no OAuth: access uses a bearer key (`xpl_` followed by hex) minted at platform.experientiallabs.ai under API Keys.

- **API-key credentials**: upload a JSON credential such as `{"type":"experiential-labs","api_key":"xpl_..."}`. A missing or malformed key is an error. Keys do not expire, so there is no refresh flow.
- **Live model catalog**: `model.for_auth` calls `GET /v1/models` with the stored key and lists the slugs that key can call. The call consumes no credits.
- **Per-model protocol routing**: each model is sent to the protocol its documentation names. Cross-protocol client translation uses `sdk/translator/builtin`.
  - **OpenAI Chat Completions** (`POST /v1/chat/completions`): `gpt-6-astra`, `glm-5.3`
  - **OpenAI Responses** (`POST /v1/responses`): implemented; no catalog model has Responses documented yet
  - **Anthropic Messages** (`POST /v1/messages`): implemented; no catalog model has Messages documented yet
  - A catalog model without a documented protocol is listed without a `type` and fails at execution with `unconfirmed_protocol`. The plugin does not guess.
- **Streaming** is passed through for all three protocols.
- **Credit balance**: `quota.fetch` reads `GET /api/v1/credits` and reports remaining credits as one `balance` bucket. The balance is org-wide.

Not implemented: `count_tokens` (returns HTTP 501), thinking-level suffixes such as `model(high)` (the docs do not name the reasoning request parameter, so these fail with `unsupported_thinking_suffix`), and a management stats page.

## Build

```bash
make test
```

Produces `build/experiential-labs.so`.

## Install from the management interface

Add this registry URL to `plugins.store-sources`:

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/esauvisky/CLIProxyAPI-Experiential-Labs-Plugin/main/registry.json"
```

## Configuration

```yaml
plugins:
  enabled: true
  configs:
    experiential-labs:
      enabled: true
      priority: 10
      base_url: "https://api.experientiallabs.ai"
```

To support another documented model, add its slug, protocol, context length and output limit to `confirmedModels` in `models.go`.

Docs used: platform.experientiallabs.ai/docs, /docs/coding-agents, /docs/reference, /docs/cost-api, /docs/spend-api, /llms.txt, and the model pages /models/gpt-6-astra and /models/glm-5.3.
