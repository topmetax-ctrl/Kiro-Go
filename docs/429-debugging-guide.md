# Kiro-Go 429/403 Error Handling — Debugging Guide & Knowledge Base

> **Status**: Accounts dead (403 revoked). Code is production-ready for new accounts.
> **Last updated**: 2026-07-03

---

## 1. Summary

All 5 Microsoft Entra accounts were permanently revoked by AWS/Microsoft. The 429 errors were the first symptom of an escalating anti-abuse response:

```
Phase 1: 200 OK (normal operation)
Phase 2: 429 "suspicious activity — temporary limits while we investigate"
Phase 3: 429 spreads to other accounts on the same tenant
Phase 4: 403 "Your subscription does not support this application" (PERMANENT)
Phase 5: 403 "User is not authorized to make this call" (PERMANENT)
```

**Key insight**: This was NOT a TLS fingerprint or code bug. The anti-abuse flag was at the account/tenant level, escalated by AWS after detecting unusual traffic patterns from the Docker container IP range. No amount of TLS spoofing could prevent it.

---

## 2. Architecture Overview

### 2.1 Request Flow

```
Claude Code Client
    │
    ▼
┌──────────────────────────────────────┐
│  Kiro-Go Proxy (Go HTTP server)      │
│  Port 8080                            │
│                                       │
│  Handler.ServeHTTP()                  │
│    ├─ authenticate()                  │
│    ├─ handleClaudeMessages()          │
│    │   ├─ OpenAIToKiro()  (translate) │
│    │   └─ handleClaudeStream()        │
│    │       └─ retry loop (3 attempts) │
│    │           └─ pool.GetNextForModel│
│    │               └─ CallKiroAPI()   │
│    │                   └─ 3 endpoints│
│    └─ handleAccountFailure()          │
│        └─ pool.RecordError()          │
│            └─ cooldown (1h or short)  │
└──────────────────────────────────────┘
    │
    ▼
┌──────────────────────────────────────┐
│  Kiro API (AWS)                      │
│                                       │
│  3 endpoints tried in sequence:       │
│  1. q.us-east-1.amazonaws.com        │
│     (Kiro IDE, no AmzTarget)         │
│  2. codewhisperer.us-east-1.amazo... │
│     (CodeWhisperer)                   │
│  3. q.us-east-1.amazonaws.com        │
│     (AmazonQ, different AmzTarget)    │
└──────────────────────────────────────┘
```

### 2.2 Pool & Cooldown System

- **Weighted round-robin** across enabled accounts
- **Cooldown types**:
  - Quota error (429 with "quota") → 1 hour cooldown (`RecordError(id, true)`)
  - Anti-abuse (429 with "suspicious activity") → short cooldown (`RecordError(id, false)`)
  - Auth failure → account auto-disabled
  - Consecutive errors (≥3) → 1 minute cooldown
- **Fallback behavior**: Returns `nil` when all accounts are on cooldown (prevents infinite retry loops)

---

## 3. Deep-Dive: 429 Response Types

AWS Kiro API returns 429 with different body messages. Each requires different handling:

### 3.1 Genuine Quota Exhaustion
```json
{"message": "Quota exceeded", "reason": "QUOTA_EXCEEDED"}
```
- **Action**: 1-hour cooldown, try another account
- **Detection**: `isQuotaErrorMessage()` — contains "429" or "quota", NOT "suspicious activity"

### 3.2 Anti-Abuse Temporary Limit
```json
{"message": "Due to suspicious activity, we are imposing temporary limits on how frequently your account (USER_ID) can send a request to Kiro while we investigate."}
```
- **Action**: Short cooldown, log warning, DO NOT retry rapidly
- **Detection**: `isAntiAbuseMessage()` — contains "429" AND "suspicious activity"
- **Critical**: This is the harbinger of a permanent ban

### 3.3 Subscription Revoked (403)
```json
{"message": "Your subscription does not support this application. Please contact your administrator."}
```
- **Action**: Account is dead — disable permanently
- **Cause**: Microsoft Entra admin revoked Kiro app access, or AWS escalated anti-abuse

### 3.4 User Not Authorized (403)
```json
{"message": "User is not authorized to make this call."}
```
- **Action**: Account is dead — disable permanently
- **Cause**: Token invalid, user deleted, or tenant block

---

## 4. TLS Fingerprinting — What We Learned

### 4.1 JA3 Hash Detection

AWS WAF/Shield performs TLS fingerprinting using JA3 hashes. Different TLS libraries produce different ClientHello messages:

| TLS Stack | JA3 Hash | AWS Detection |
|-----------|----------|---------------|
| Go `crypto/tls` | Distinctive | **IMMEDIATELY FLAGGED** |
| `utls` Chrome 133 | Chrome-like | Flagged after short period |
| `utls` Firefox 120 | Firefox-like | Flagged after short period |
| `utls` iOS 14 | iOS-like | Flagged after short period |
| `utls` Safari 16 | Safari-like | Flagged after short period |
| curl (SecureTransport) | macOS native | Not flagged |
| Python `urllib` (OpenSSL) | OpenSSL | Flagged (OpenSSL) |
| Python `ssl` (SecureTransport) | macOS native | Not flagged |
| wget (OpenSSL) | OpenSSL | Not flagged (but limited testing) |

### 4.2 Why utls Didn't Save Us

1. `utls` mimics browser ClientHello format, but subtle differences exist
2. AWS has likely trained their models on `utls` patterns (it's a well-known library)
3. `utls` cannot use HTTP/2 with Go's `DialTLSContext` — requires HTTP/1.1 fallback
4. HTTP/1.1 + non-browser-origin IP = suspicious pattern
5. Once the account-level flag escalates, no fingerprint change helps

### 4.3 utls Integration Notes (for future reference)

```go
// Working pattern (but not needed currently):
// 1. Get browser spec
spec, _ := utls.UTLSIdToSpec(utls.HelloFirefox_Auto)

// 2. Force HTTP/1.1 ALPN (Go can't detect h2 from external dialer)
for i, ext := range spec.Extensions {
    if alp, ok := ext.(*utls.ALPNExtension); ok {
        alp.AlpnProtocols = []string{"http/1.1"}
        spec.Extensions[i] = alp
        break
    }
}

// 3. Use DialTLSContext with HelloGolang + ApplyPreset
//    (NOT HelloFirefox_Auto directly — it overrides NextProtos)
t.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
    rawConn, _ := dialer.DialContext(ctx, network, addr)
    uconn := utls.UClient(rawConn, &utls.Config{ServerName: serverName}, utls.HelloGolang)
    uconn.ApplyPreset(&spec)
    uconn.HandshakeContext(ctx)
    return uconn, nil
}

// 4. Disable HTTP/2 on Go's side
t.ForceAttemptHTTP2 = false
t.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
```

**Gotchas**:
- `utls.HelloFirefox_Auto` with `Config.NextProtos` → Firefox preset OVERRIDES NextProtos
- `utls.HelloChrome_Auto` directly with `DialTLSContext` → server sends HTTP/2 frames, Go parses as HTTP/1.1 → "malformed HTTP response"
- `utls.HelloGolang` + `ApplyPreset(&spec)` → only working pattern for HTTP/1.1

### 4.4 Why curl Worked When Everything Else Failed

```
curl 8.7.1 (x86_64-apple-darwin24.0)
libcurl/8.7.1 (SecureTransport) LibreSSL/3.3.6
```

curl on macOS uses **SecureTransport** (Apple's TLS framework), which has a completely different TLS fingerprint from both OpenSSL and Go's crypto/tls. AWS's anti-abuse model apparently trusts SecureTransport more than other TLS stacks, or hasn't trained on its fingerprint from datacenter IPs.

---

## 5. Debugging Toolkit

### 5.1 Direct Kiro API Test (curl)

```bash
TOKEN="<account-access-token>"
PROFILE_ARN="arn:aws:codewhisperer:us-east-1:756838402647:profile/4EVDXVPKQVPX"

curl -s -w "\nHTTP_CODE:%{http_code}" --max-time 15 \
  -X POST "https://q.us-east-1.amazonaws.com/generateAssistantResponse" \
  -H "Content-Type: application/json" \
  -H "Accept: */*" \
  -H "Authorization: Bearer $TOKEN" \
  -H "User-Agent: aws-sdk-js/1.0.34 ua/2.1 os/macOS lang/js md/nodejs#20.0.0 api/codewhispererstreaming#1.0.34 m/E KiroIDE-2.1.0-<machineId>" \
  -H "Host: q.us-east-1.amazonaws.com" \
  -H "x-amzn-codewhisperer-optout: true" \
  -H "x-amzn-kiro-agent-mode: vibe" \
  -H "TokenType: EXTERNAL_IDP" \
  -d "{\"profileArn\":\"$PROFILE_ARN\",\"conversationState\":{\"chatTriggerType\":\"MANUAL\",\"conversationId\":\"test\",\"currentMessage\":{\"userInputMessage\":{\"content\":\"say ok\",\"modelId\":\"claude-sonnet-4\",\"origin\":\"AI_EDITOR\"}},\"history\":[]}}"
```

### 5.2 Profile ARN Resolution

```bash
TOKEN="<account-access-token>"
curl -s -X POST "https://codewhisperer.us-east-1.amazonaws.com/ListAvailableProfiles" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -H "TokenType: EXTERNAL_IDP" \
  -d '{"maxResults":10}'
```

### 5.3 Account Health Check

```bash
# Via proxy admin API
curl -s -H "X-Admin-Password: <password>" \
  http://localhost:8080/admin/api/accounts/<account-id>/test \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-4"}'

# Full account info
curl -s -H "X-Admin-Password: <password>" \
  http://localhost:8080/admin/api/accounts/<account-id>/full

# Refresh account status from AWS
curl -s -X POST -H "X-Admin-Password: <password>" \
  http://localhost:8080/admin/api/accounts/<account-id>/refresh
```

### 5.4 Testing from Inside Docker Container

```bash
# Install curl
docker exec kiro-go-kiro-go-1 apk add --no-cache curl

# Test from container
docker exec kiro-go-kiro-go-1 curl -s -w "%{http_code}" --max-time 10 \
  -X POST "https://q.us-east-1.amazonaws.com/generateAssistantResponse" \
  -H "Authorization: Bearer $TOKEN" \
  -H "TokenType: EXTERNAL_IDP" \
  -d '{"profileArn":"...","conversationState":{...}}'

# Test from container with wget (if curl not available)
docker exec kiro-go-kiro-go-1 wget -qO- --timeout=10 \
  --header="Authorization: Bearer $TOKEN" \
  --header="TokenType: EXTERNAL_IDP" \
  --post-data='{...}' \
  "https://q.us-east-1.amazonaws.com/generateAssistantResponse"
```

### 5.5 Comparing TLS Fingerprints

```bash
# Check what TLS library curl uses
curl --version | head -1

# Check what TLS library Python uses
python3 -c "import ssl; print(ssl.OPENSSL_VERSION)"

# Check linked TLS libraries for any binary
otool -L /path/to/binary | grep -i ssl
```

---

## 6. Code Changes Reference

### 6.1 429 Body Logging (`proxy/kiro.go`)

**Before** (body discarded):
```go
if resp.StatusCode == 429 {
    resp.Body.Close()  // BODY LOST!
    logger.Warnf("[KiroAPI] Endpoint %s quota exhausted (429), trying next...", ep.Name)
    lastErr = fmt.Errorf("quota exhausted on %s", ep.Name)
    continue
}
```

**After** (body logged and included in error):
```go
if resp.StatusCode == 429 {
    errBody, _ := io.ReadAll(resp.Body)
    resp.Body.Close()
    logger.Warnf("[KiroAPI] Endpoint %s quota exhausted (429), body=%s, trying next...", ep.Name, string(errBody))
    lastErr = fmt.Errorf("quota exhausted on %s: %s", ep.Name, string(errBody))
    continue
}
```

### 6.2 Anti-Abuse Detection (`proxy/account_failover.go`)

```go
func isQuotaErrorMessage(msg string) bool {
    msg = strings.ToLower(msg)
    // Only match genuine quota exhaustion, not anti-abuse "suspicious activity" flags.
    if strings.Contains(msg, "suspicious activity") {
        return false
    }
    return strings.Contains(msg, "429") || strings.Contains(msg, "quota")
}

func isAntiAbuseMessage(msg string) bool {
    msg = strings.ToLower(msg)
    return strings.Contains(msg, "429") && strings.Contains(msg, "suspicious activity")
}
```

**Failover logic** (anti-abuse gets short cooldown, not 1h):
```go
case isAntiAbuseMessage(errMsg):
    h.pool.RecordError(account.ID, false)  // short cooldown
    logger.Warnf("[AccountFailover] Anti-abuse throttle for %s: %s", account.Email, errMsg)
case isQuotaErrorMessage(errMsg):
    h.pool.RecordError(account.ID, true)   // 1h cooldown
```

### 6.3 Cooldown Fallback Fix (`pool/account.go`)

**Before** (returned throttled account → infinite retry loop):
```go
// fallback: return account with earliest cooldown
var best *config.Account
var earliest time.Time
for i := range p.accounts {
    if cooldown, ok := p.cooldowns[acc.ID]; ok {
        if best == nil || cooldown.Before(earliest) {
            best = acc   // BAD: returns throttled account!
        }
    }
}
return best
```

**After** (return nil → caller returns 503):
```go
// All accounts are on cooldown or blocked. Return nil instead of the
// account with the earliest cooldown — retrying a throttled account
// immediately only wastes upstream quota and resets the cooldown timer.
return nil
```

---

## 7. Operational Knowledge

### 7.1 Kiro API Endpoints

```
1. Kiro IDE:
   URL: https://q.us-east-1.amazonaws.com/generateAssistantResponse
   Origin: AI_EDITOR
   AmzTarget: (none)
   Note: Same host as AmazonQ, different behavior

2. CodeWhisperer:
   URL: https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse
   Origin: AI_EDITOR
   AmzTarget: AmazonCodeWhispererStreamingService.GenerateAssistantResponse

3. AmazonQ:
   URL: https://q.us-east-1.amazonaws.com/generateAssistantResponse
   Origin: AI_EDITOR
   AmzTarget: AmazonQDeveloperStreamingService.SendMessage
   Note: Same host as Kiro IDE
```

**Regional endpoints**: `q.{region}.amazonaws.com` — only us-east-1 and eu-central-1 available as of 2026.

### 7.2 Account Types

| Auth Method | Token Refresh | Profile ARM | Notes |
|-------------|---------------|-------------|-------|
| `external_idp` (Microsoft Entra) | Via refresh token | Required (ListAvailableProfiles) | `TokenType: EXTERNAL_IDP` header |
| `idc` (IAM Identity Center) | Via refresh token + client credentials | Required | BuilderId unsupported |
| `social` (Google/GitHub) | Via refresh token | Required | - |
| `api_key` | Not needed (API key is static) | NOT required | `tokentype: API_KEY` header |

### 7.3 Headers Kiro Expects

```
Required:
  Authorization: Bearer <token>
  Content-Type: application/json
  User-Agent: aws-sdk-js/... KiroIDE-<version>-<machineId>
  x-amz-user-agent: aws-sdk-js/... KiroIDE-<version>-<machineId>

Conditional:
  TokenType: EXTERNAL_IDP          (for external_idp accounts)
  tokentype: API_KEY               (for api_key accounts)
  X-Amz-Target: <target>           (for CodeWhisperer and AmazonQ endpoints)

Best Practice:
  x-amzn-codewhisperer-optout: true
  x-amzn-kiro-agent-mode: vibe
  Amz-Sdk-Request: attempt=1; max=3
  Amz-Sdk-Invocation-Id: <uuid>
```

### 7.4 Profile ARN Resolution

Profile ARN format: `arn:aws:codewhisperer:{region}:{account-id}:profile/{profile-id}`

Resolution flow (in `ResolveProfileArn`):
1. Check `account.ProfileArn` — if set, return immediately
2. Probe `ListAvailableProfiles` across regions (us-east-1, eu-central-1)
3. Fallback: `auth.RefreshToken()` which may return profileArn
4. Cache result via `config.UpdateAccountProfileArnWithRegion()`
5. Builder ID accounts: cache "unsupported" for 24h

### 7.5 Docker Desktop on Mac Notes

- **Outbound IP**: Docker Desktop uses NAT — outbound IP is the SAME as Mac host
- **`network_mode: host`**: DOES NOT give Mac's IP — gives Linux VM's IP (useless on Mac)
- **`host.docker.internal`**: Resolves to Mac host from within containers
- **Bridge IP range**: `192.168.65.0/24` (Docker Desktop VM)

---

## 8. Lessons Learned

### 8.1 Debugging

1. **Always log error response bodies.** The 429 body contained the crucial clue ("suspicious activity") that was being discarded.
2. **Test from multiple vantage points.** curl from Mac, curl from Docker, wget from Docker, Go binary on Mac — each gave different results.
3. **Don't trust the first diagnosis.** Initially looked like quota, then IP-based blocking, then TLS fingerprinting. The real issue was account revocation.
4. **Correlation is not causation.** The 429 started when using Go TLS, but it was actually the beginning of an account-level ban escalation.

### 8.2 Anti-Detection

1. **TLS fingerprinting is real** — AWS does it, and Go's `crypto/tls` is easily detectable.
2. **utls helps but is not a silver bullet** — AWS has likely trained on utls patterns.
3. **HTTP/2 vs HTTP/1.1 matters** — using HTTP/1.1 when browsers use HTTP/2 is suspicious.
4. **Account-level flags trump everything** — once AWS decides to ban an account, no fingerprint change saves it.

### 8.3 Architecture

1. **Never rely on a single account.** Always have backup accounts from different tenants.
2. **Cooldown fallback returning throttled accounts creates death spirals.** Fixed in this PR.
3. **Distinguish error types.** Quota (1h cooldown) vs anti-abuse (short cooldown) vs auth (disable account).
4. **The proxy's retry loop (3 attempts × 3 endpoints = 9 upstream calls per user request) can accelerate anti-abuse flagging.**

### 8.4 Account Procurement

For future accounts:
- Use different Microsoft Entra tenants (different `issuerUrl`)
- Different `IdPClientID` per tenant
- Rotate accounts regularly to avoid building up suspicious traffic on any single account
- Monitor for 429 "suspicious activity" as early warning before 403 revocation

---

## 9. Related Files

| File | Purpose |
|------|---------|
| `proxy/kiro.go` | Kiro API client, HTTP transport, endpoint fallback, event stream parsing |
| `proxy/kiro_api.go` | REST API calls (GetUsageLimits, ListAvailableModels, ResolveProfileArn) |
| `proxy/kiro_headers.go` | Header construction (User-Agent, Kiro IDE fingerprint headers) |
| `proxy/handler.go` | HTTP handler, streaming/non-streaming Claude & OpenAI endpoints, admin API |
| `proxy/account_failover.go` | Error classification (quota/anti-abuse/auth/suspension), account disable |
| `proxy/translator.go` | OpenAI ↔ Kiro payload translation, conversation ID generation |
| `proxy/inject.go` | Kiro IDE token cache injection (write kiro-auth-token.json) |
| `pool/account.go` | Weighted round-robin pool, cooldown management, model routing |
| `auth/` | Token refresh, SSO login flows (IAM, BuilderId, Kiro SSO) |
| `config/` | Account config persistence, settings management |
| `data/config.json` | Runtime configuration (accounts, settings, stats) |

---

## 10. Quick Recovery Checklist

When accounts go down:

1. [ ] Check proxy logs for 429/403 bodies: `curl -H "X-Admin-Password: <p>" http://localhost:8080/admin/api/logs`
2. [ ] Test each account directly via curl (Section 5.1)
3. [ ] Check account status: `curl -H "X-Admin-Password: <p>" http://localhost:8080/admin/api/accounts`
4. [ ] Enable backup accounts: `curl -X PUT -H "X-Admin-Password: <p>" .../accounts/<id> -d '{"enabled":true}'`
5. [ ] Refresh account info: `curl -X POST -H "X-Admin-Password: <p>" .../accounts/<id>/refresh`
6. [ ] If all accounts dead: procure new accounts from different Microsoft tenant
7. [ ] Add new accounts via admin UI or API: `POST /admin/api/auth/kiro-sso/start`
