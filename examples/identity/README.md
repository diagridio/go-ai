# identity example

A two-route HTTP service that verifies the inbound Catalyst user token and
propagates the caller's identity on an outbound call. Deliberately minimal —
no agent, no model, no workflow, just identity.

## How it works

Two lines of app code buy verified inbound identity:

```go
oauth := identity.OAuthConfig{}
handler := identity.Middleware(oauth)(mux)
```

The zero `OAuthConfig` is fail-closed: it requires a verified token on every
request and discovers the issuer, audience and JWKS endpoint from the sidecar,
so there is nothing else to configure. It requires no particular scope — scopes
are demonstrated in the handler instead, which keeps the example runnable
without scope setup.

Three things follow from that:

1. **Inbound verification** — the middleware wraps the mux, so a request
   without a good token never reaches a handler.
2. **Reading the caller** — `GET /whoami` reads the verified caller through the
   typed accessor `identity.UserFromContext(ctx)` and reports its `subject`,
   `tenant`, `scopes`, and `HasScope("read")` as `hasRead`.
3. **Outbound propagation** — `GET /downstream` makes one ordinary GET to
   `DOWNSTREAM_URL` (default `http://localhost:8081/whoami`) through the
   identity-aware client, so the callee sees the same caller. It returns
   `{"downstream": "<the body it got back>"}`, or 502
   `{"error":"downstream_unreachable"}` — this example's own code, not an SDK
   one.

The client is constructed once, at startup, and shared by every request:

```go
client := identity.NewHTTPClient(nil)
```

The handler then has no identity code in it at all — it builds the outbound
request with the inbound request's context and calls `client.Do(req)`. The
caller's token is read from that context at send time, which is what makes one
shared client safe: two concurrent callers each carry their own token. It is a
plain `*http.Client`, so it also goes straight into anything that takes one (an
MCP client, a generated API client).

## Run it

This example runs under its own `identity-demo` App ID (set in `catalyst.yaml`),
so it doesn't overlap the other examples.

```bash
go mod tidy
diagrid agent create identity-demo --project $PROJECT --wait
diagrid dev run -f catalyst.yaml --project $PROJECT
```

Or skip the run file and pass the same settings inline:

```bash
diagrid dev run --project $PROJECT --app-id identity-demo -e GOWORK=off -- go run .
```

To watch the propagation, run a second copy on 8081 and point the first at it:

```bash
DOWNSTREAM_URL=http://localhost:8081/whoami diagrid dev run -f catalyst.yaml --project $PROJECT
```

## Try it

The sidecar sets the token on requests it forwards, so the success cases are
best driven through it (`diagrid call invoke` or an invoke from the console):

```bash
curl -s localhost:8080/whoami -H "X-Diagrid-User-Token: Bearer $TOKEN"
{"subject":"you@example.com","tenant":"acme","scopes":["agent.invoke","read"],"hasRead":true}

curl -s localhost:8080/downstream -H "X-Diagrid-User-Token: Bearer $TOKEN"
{"downstream":"{\"subject\":\"you@example.com\",\"tenant\":\"acme\",\"scopes\":[\"read\"],\"hasRead\":true}\n"}
```

Every rejection has the same `{"error":"<code>"}` body shape:

```bash
# no token -> 401
curl -s -i localhost:8080/whoami | tail -1
{"error":"oauth.missing_token"}

# garbage token -> 401
curl -s localhost:8080/whoami -H "X-Diagrid-User-Token: Bearer not-a-jwt"
{"error":"oauth.decode_error"}

# expired token -> 401
curl -s localhost:8080/whoami -H "X-Diagrid-User-Token: Bearer $EXPIRED"
{"error":"oauth.expired"}
```

## Notes

- The token arrives in the `X-Diagrid-User-Token` header, which the Catalyst
  sidecar sets on every request it forwards. It is deliberately not
  `Authorization`: that header stays yours.
- `RequireAuth` stays unset here, which resolves to true. Set it to `false`
  (it's a `*bool`, so `open := false; OAuthConfig{RequireAuth: &open}`) when
  health and readiness endpoints have to share the same app — a token that is
  present is still verified either way.
- `AllowInsecureJWKS` permits a JWKS endpoint served over plain http, for local
  development against a non-loopback sidecar. It relaxes the rule to http and
  nothing else — a `file://` URI stays refused with the flag set. It must not be
  set in production: the key set is the root of trust, and an on-path attacker
  who rewrites a plaintext response can mint tokens this package accepts.
- `scopes` comes back deduplicated and ordinally sorted rather than in the order
  the token listed them, which is what every Diagrid SDK reports for the same
  token.
- The token goes only to the origin the request addressed: if `DOWNSTREAM_URL`
  answers with a redirect to another host, the client drops the header rather
  than hand the caller's token to whatever that redirect names.
- Outside a verified request — a cron or pub/sub trigger, where there is no
  caller — the call still goes out, unauthenticated, with the header omitted
  rather than sent empty. That is not an error.
- Without a sidecar to discover identity coordinates from, a request carrying a
  token answers 503 `{"error":"oauth.not_configured"}` — no verifier could be
  built. A tokenless request still answers 401 `oauth.missing_token`.
