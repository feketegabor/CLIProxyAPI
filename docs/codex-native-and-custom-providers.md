# Native ChatGPT and custom model routing

Codex selects a provider separately from a model. A model catalog's display name
does not select credentials, a subscription, or a network destination. Putting
native GPT models into a proxy catalog does not make them use native ChatGPT.

## Preserve the native default

Keep the user's ChatGPT sign-in. Do not replace or delete `auth.json`.
Remove global `model_provider`, `openai_base_url`, and `model_catalog_json`
overrides to use the built-in provider and supplied catalog. Preserve unrelated
settings and take private backups before making changes.

For opt-in custom inference, current Codex versions support a separate
`$CODEX_HOME/custom_models.config.toml` file:

```toml
model_provider = "cliproxy"
model = "your-verified-custom-model"
model_catalog_json = "/absolute/path/to/custom-providers.json"

[model_providers.cliproxy]
name = "CLIProxyAPI"
base_url = "http://127.0.0.1:8317/v1"
wire_api = "responses"
env_key = "CLIPROXY_API_KEY"
requires_openai_auth = false
supports_websockets = false
```

Use `codex --profile custom_models`. Supply the proxy credential privately in
the process environment; do not commit it. The example uses proxy authentication,
not ChatGPT authentication. It does not guarantee ChatGPT-specific app tools in
a custom-provider session. Existing app configurations using
`requires_openai_auth = true` need their authentication headers audited separately:
that setting can send OpenAI authentication to the configured provider and is not
a per-model routing switch.

The Desktop model selector is not a provider selector. In the inspected app-server
protocol, thread start/resume accepts `modelProvider`, but turn start changes only
the model. Therefore a single mixed selector requires provider-aware client
behavior. Do not invent a provider field in catalog entries or advertise native
models as native while routing them through this proxy. Existing tasks may retain
their previous provider after the global configuration changes.

## Remove an extra gateway only after verification

The server handles compressed request bodies and standalone delegation history.
This branch also preserves custom tool calls and translates client tool discovery
for Chat Completions, Messages, Responses, and Gemini provider paths. The header
helper derives `x-opencode-session` from the caller's session headers when the
configured forwarded header is absent.

Before retiring a gateway, verify the deployed binary's commit and test the direct
route with streaming, shell/tool-result continuation, native `apply_patch`, tool
discovery, and separate concurrent sessions. A models-list response alone is not
proof of compatibility. Keep backups and the previous startup configuration for
rollback; do not disable a gateway while a running repair task still needs it.

Bind a local-only installation to `127.0.0.1`. Use a dedicated proxy access key;
do not reuse a provider's upstream API key as the client-facing key.

## Provider provenance and OAuth

Build each machine's catalog from routes available on that machine. Do not copy
homeserver entries to a Windows machine without their corresponding providers.
Distinguish `grok-4.6` through xAI subscription authentication from
`opencode-go-grok-4.6` through OpenCode Go. Do not silently fall back between their
quotas. Advertise an OAuth-backed model only after authentication and a live call.

Run OAuth on the machine hosting the proxy. A loopback callback on a Mac cannot
reach a Windows listener without an explicit tunnel. `--no-browser` does not
necessarily mean device-code authentication. Inspect the selected provider's
current login implementation and let the user complete account sign-in/consent.
Never move another machine's credential store as a substitute for authorization.

References:
- https://developers.openai.com/codex/config-reference/
- https://developers.openai.com/codex/config-advanced/#profiles
- https://developers.openai.com/codex/app-server/
