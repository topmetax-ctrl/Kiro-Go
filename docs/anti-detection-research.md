# Anti-Detection Deep Research: Kiro-Go Project

> Research date: 2026-07-03
> Context: Kiro-Go is a Go proxy for AWS Kiro API (CodeWhisperer/Amazon Q) getting 429 "suspicious activity" errors from AWS anti-abuse systems

---

## Executive Summary

AWS WAF/Shield employs a **multi-layer fingerprinting stack** to detect non-browser API traffic:

| Layer | Technology | What AWS Sees (Kiro-Go) | Status |
|-------|-----------|------------------------|--------|
| **TCP/IP** | JA4T / p0f signatures | Docker Linux VM (TTL=64, window=64240, Linux TCP options order) | ❌ Detectable |
| **TLS** | JA3/JA4 hash | Go `crypto/tls` → distinct JA4 hash (even with uTLS Safari, still non-native) | ⚠️ Partial |
| **HTTP/2** | SETTINGS frames, header order | Go's H2 implementation differs from browsers | ❌ Not addressed |
| **Behavior** | Request rate, pattern, timing | Multi-endpoint retry bursts trigger rate limiting | ❌ Current issue |

**Key finding**: uTLS alone is insufficient. AWS added JA4 fingerprinting in March 2025, and the Kiro API specifically uses cross-layer detection (TCP + TLS + HTTP + behavior).

---

## 1. How AWS Detects Non-Browser Traffic

### 1.1 JA3/JA4 TLS Fingerprinting

AWS officially announced **JA4 fingerprinting** in March 2025 for WAF rate-based rules (CloudFront, ALB, all regions). The JA4 hash is a 36-character structured hash computed from:
- TLS version
- Cipher suite list (sorted, unlike JA3)
- Extension list (sorted)
- ALPN values (sorted)

Standard HTTP libraries produce **trivially distinguishable** fingerprints:
- **Go `net/http`**: Unique JA4 hash, easily blocklisted
- **Python `requests`**: OpenSSL fingerprint, different from browsers
- **Node `axios`**: Distinct from browser NodeJS
- **curl (SecureTransport on macOS)**: Matches Safari → passes detection
- **curl (OpenSSL on Linux)**: Distinct fingerprint → may be flagged

> **Source**: [AWS WAF adds JA4 fingerprinting (March 2025)](https://aws.amazon.com/cn/about-aws/whats-new/2025/03/aws-waf-ja4-fingerprinting-aggregation-ja3-ja4-fingerprints-rate-based-rules/) · [DoiT: JA3 and JA4 in AWS WAF](https://www.doit.com/blog/ja3-and-ja4-fingerprints-in-aws-waf-and-beyond)

### 1.2 JA4T TCP/IP Fingerprinting

**This is the critical finding.** AWS and anti-abuse systems can fingerprint the TCP stack itself, operating BELOW the TLS layer:

```
JA4T = f(Window Size, TCP Options Order, Window Scale)
```

**Per-OS TCP SYN signatures**:

| OS | Window Size | TTL | TCP Options Order (option kinds) | Timestamps? |
|----|------------|-----|----------------------------------|-------------|
| **Linux** (Docker) | 64240 | 64 | 2-4-8-1-3 (MSS, SACK, TS, NOP, WSCALE) | Yes |
| **macOS** | 65535 | 64 | 2-1-3-1-1-8-4 (MSS, NOP, WSCALE, NOP, NOP, TS, SACK) | Yes |
| **Windows** | 65535 | 128 | 2-1-3-1-1-4 (MSS, NOP, WSCALE, NOP, NOP, SACK) | **No** |

**Critical implications for Kiro-Go**:
1. **Docker Desktop's Linux VM** presents a clean Linux TCP fingerprint → trivially distinguishable from macOS browser traffic
2. **When traffic passes through a proxy**, AWS sees the PROXY's TCP fingerprint, not the client's
3. **Cross-layer mismatch detection**: If TLS says "Chrome on macOS" but TCP says "Linux kernel", it's an instant red flag
4. **MSS reveals tunneling**: MSS 1424 = unencrypted proxy, MSS 1380 = VPN/tunnel

> **Source**: [FoxIO: JA4T TCP Fingerprinting](https://foxio.io/blog/ja4t-tcp-fingerprinting) · [pydoll: Network Fingerprinting](https://pydoll.tech/docs/deep-dive/fingerprinting/network-fingerprinting/) · [GoLogin: Device Fingerprinting 2026](https://gologin.com/blog/device-fingerprinting/)

### 1.3 The 429 Response

`429 Too Many Requests` with body `"Due to suspicious activity, we are imposing temporary limits..."` is AWS's standard response when **rate-limit rules fire on suspicious JA3/JA4 fingerprints**. This is NOT the same as:
- Account quota exhaustion (usageCurrent >= usageLimit) → different error
- Subscription revocation (403 "does not support") → auth error
- Token invalidity (403 "invalid token") → auth error

> **Source**: [CapMonster: Bypass Anti-Bot TLS Fingerprinting](https://capmonster.cloud/blog/how-to-bypass-antibots-tls-fingerprinting-captcha-handling/)

---

## 2. uTLS: Effectiveness and Limitations

### 2.1 What uTLS Can Do

uTLS operates at the **TLS ClientHello message level only**. It spoofs:
- Cipher suite list and ordering
- TLS extensions and their ordering
- ALPN negotiation values
- Signature algorithms
- Supported groups (elliptic curves)
- Compression methods

For Kiro-Go, we implemented Safari/iOS fingerprint via `HelloSafari_Auto` with HTTP/1.1-only ALPN.

### 2.2 What uTLS CANNOT Do (Detectable Gaps)

| Layer | Gap | Detection Risk |
|-------|-----|---------------|
| **TLS Record Layer** | Framing patterns, fragment sizes | Medium |
| **HTTP/2 SETTINGS** | Frame values, WINDOW_UPDATE, priority tree | High |
| **TCP Stack** | Window size, options order, TTL, MSS | **Critical** |
| **Certificate Verification** | OCSP stapling, CRL behavior | Low |
| **Session Resumption** | Timing, ticket format | Low |
| **Application Data** | Request timing, inter-request delays | High |

> **Source**: [DeepWiki: uTLS Architecture](https://deepwiki.com/refraction-networking/utls/1.1-features-and-architecture)

### 2.3 Known uTLS Detection History

- **Issue #321**: Chrome/Firefox imitations actively blocked, Go's default crypto/tls actually worked better in some cases
- **v1.8.1**: Fixed "critical issue that could cause simulated Chrome fingerprints to be detected" — proving that uTLS fingerprints have had detectable flaws
- **Cat-and-mouse nature**: uTLS requires ongoing patches as fingerprinters evolve

> **Source**: [uTLS Issue #321](https://github.com/refraction-networking/utls/issues/321)

### 2.4 Kiro-Go Test Results

| Configuration | TCP Stack | TLS Stack | REST API | Streaming API |
|--------------|-----------|-----------|----------|---------------|
| Docker + Go std TLS | Linux VM | Go crypto/tls | ❌ 429 | ❌ 429 |
| Docker + uTLS Safari | Linux VM | uTLS Safari | ✅ OK | ❌ 429 |
| Mac native + Go std TLS | macOS | Go crypto/tls | ❌ 429 | ❌ 429 |
| Mac native + uTLS Safari | macOS | uTLS Safari | ❌ 429 | ❌ 429 |
| Mac curl | macOS | SecureTransport | ✅ OK (400) | ✅ OK (403) |
| Mac Python | macOS | OpenSSL | ✅ OK | ✅ OK (403) |

**Analysis**: The REST API works through Docker+uTLS but streaming doesn't. This indicates:
1. Streaming API (`GenerateAssistantResponse`) has **stricter detection** than REST (`ListAvailableModels`)
2. Anti-abuse is **per-API-method**, not just per-TLS-connection
3. After heavy test bursts, accounts get **method-specific temp bans**

---

## 3. TCP/IP Fingerprinting: The Hidden Layer

### 3.1 Docker's Linux VM Fingerprint

Docker Desktop on Mac runs a lightweight Linux VM. Its TCP stack presents:
- **Window size**: 64240 (Linux default, not macOS 65535)
- **TCP options order**: Linux pattern (2-4-8-1-3)
- **TTL**: 64 (same as macOS, but different from Windows 128)
- **MSS**: Dependent on Docker network MTU

Even if uTLS perfectly mimics Safari TLS, the TCP fingerprint betrays a **Linux kernel** — creating a cross-layer mismatch with any macOS/iOS browser fingerprint.

### 3.2 Cross-Layer Mismatch Detection

AWS can correlate:
```
TCP fingerprint (Linux) ≠ TLS fingerprint (Safari on macOS) → RED FLAG
TCP fingerprint (Linux) ≠ HTTP User-Agent (Chrome on Windows) → RED FLAG
```

To pass undetected, ALL layers must be consistent:
```
TCP (macOS) + TLS (Safari) + HTTP/2 (Safari) + Headers (macOS Safari) = CONSISTENT
```

### 3.3 Why curl from Mac Works

curl on macOS uses:
- **TCP**: macOS kernel TCP stack (native, not VM)
- **TLS**: SecureTransport (macOS native TLS library)
- **This combination is bit-identical to Safari browser traffic** at the network layer

The TLS fingerprint alone isn't sufficient — it's the **combination of native TCP + native TLS** that passes.

> **Source**: [Scrapfly: TCP/IP Stack Fingerprinting](https://scrapfly.io/blog/posts/tcp-ip-stack-fingerprinting-proxy-bypass)

---

## 4. Alternative Approaches

### 4.1 curl-impersonate (Highest Fidelity)

**Approach**: Compile curl against REAL browser TLS libraries:
- Chrome → BoringSSL
- Firefox → NSS
- Safari → SecureTransport (macOS only)

**Why it works**: Produces **bit-identical TLS ClientHello** to the target browser because it uses the SAME TLS library.

**Python binding** (`curl_cffi`) is widely used to bypass:
- Cloudflare (including 5-second shield)
- DataDome
- Akamai
- AWS WAF

**Integration with Kiro-Go**: Run as a sidecar reverse proxy — curl-impersonate terminates TLS, forwards plain HTTP to Go proxy. This completely sidesteps Go's TLS fingerprint problem.

> **Source**: [curl-impersonate](https://github.com/lwthiker/curl-impersonate) · [DeepWiki: TLS/HTTP2 Fingerprinting](https://deepwiki.com/lwthiker/curl-impersonate/4.3-tls-and-http2-fingerprinting)

### 4.2 tlsc: Full-Stack Go HTTP Client

Go library that spoofs ALL protocol layers:
- TLS (JA3/JA4) via uTLS
- HTTP/2 (Akamai SETTINGS, priorities, header order)
- HTTP/3 QUIC transport parameters
- Preset profiles: Chrome 143, Firefox 146, Safari 26.1
- Built-in 429 retry with exponential backoff

**Advantage over raw uTLS**: Addresses HTTP/2 fingerprinting, which uTLS ignores.

> **Source**: [tlsc on pkg.go.dev](https://pkg.go.dev/github.com/Phrasing/tlsc@v1.0.0)

### 4.3 go-bypasser: Drop-in RoundTripper

Wraps uTLS + tls-client into a standard `http.RoundTripper`:
- JA3, JA4, HTTP/2 Akamai fingerprint spoofing
- Browser User-Agent matching
- Optional headless Chromium mode for JS challenges

**Advantage**: Drop-in replacement for `http.DefaultTransport` — minimal code changes to Kiro-Go.

> **Source**: [go-bypasser on GitHub](https://github.com/skycheung803/go-bypasser)

### 4.4 mic: Modular Go Proxy Reference

A Go proxy using uTLS to spoof browser TLS fingerprints:
- Operates in client-front (MitM) and server-front modes
- Provides JA4 hash verification per profile
- Deployable as a forward proxy

**Relevance**: Architectural reference for Kiro-Go's anti-detection layer.

> **Source**: [mic on pkg.go.dev](https://pkg.go.dev/github.com/djnnvx/mic)

### 4.5 Kiro Community Solutions

**CLIProxyAPIPlus** (ClubWeGo community fork):
- Specifically built for Kiro/AWS CodeWhisperer
- Multi-account rotation on 429
- Configurable backoff and cooldown
- Region switching (us-east-1 ↔ us-west-2)
- Device fingerprint generation
- OAuth token management

> **Source**: [CLIProxyAPIPlus on GitHub](https://github.com/ClubWeGo/CLIProxyAPIPlus)

**Kiro Pro AWS Soft-Ban Workaround Guide** (80aj.com):
- Documents the exact 429 "suspicious activity" problem
- Recommends reverse proxy with different routing than official IDE

> **Source**: [Kiro AWS Bypass Guide](https://www.80aj.com/2026/05/22/kiro-aws-bypass-guide/)

### 4.6 Subprocess-Based Escalation (recurl Pattern)

A layered approach:
1. Try plain request
2. On 429 → escalate to curl-impersonate subprocess (browser TLS)
3. On persistent block → escalate to headless Chromium

This offloads TLS to a native-compiled binary using real browser TLS, producing genuinely browser-identical ClientHello bytes.

> **Source**: [recurl: Escalation-Layered HTTP Client](https://github.com/neul-labs/recurl)

---

## 5. Academic Research: Beyond TLS Fingerprinting

**USENIX Security 2024** (Xue et al.) demonstrated that even when outer TLS is perfectly spoofed with uTLS, censors can detect proxied traffic by analyzing:
- Packet size patterns
- Timing between packets
- Direction patterns (upstream vs downstream)
- Inner TLS handshake characteristics inside tunnels

**Results**: TPR > 70% against all tested obfuscated proxies, FPR as low as 0.054%.

**Countermeasure**: Traffic multiplexing (mixing multiple streams) is the most promising defense.

**Implication for Kiro-Go**: If AWS is performing deep traffic analysis, no TLS spoofing library alone will suffice. The proxy would need traffic shaping (jitter, padding, multiplexing).

> **Source**: [USENIX Security 2024: Encapsulated TLS Handshake Fingerprinting](https://www.usenix.org/system/files/usenixsecurity24-xue-fingerprinting.pdf)

---

## 6. Recommended Action Plan for Kiro-Go

### Immediate (Already Done)
- [x] uTLS Safari/iOS fingerprint integration
- [x] JA3/JA4 TLS ClientHello spoofing
- [x] 429 body logging (was discarding error details)
- [x] Account cooldown fallback fix (return nil instead of throttled account)
- [x] Anti-abuse vs quota error differentiation

### Short-Term (This Week)

1. **Add curl-impersonate sidecar proxy**
   - Run curl-impersonate (Chrome BoringSSL) on Mac host as TLS terminator
   - Go proxy connects via HTTP to sidecar → curl-impersonate does TLS to Kiro
   - This gives **bit-identical browser TLS fingerprint** + **macOS TCP stack**
   - Docker → HTTP localhost:8899 → curl-impersonate → HTTPS Kiro API

2. **Implement TCP fingerprint awareness**
   - Run the proxy natively on Mac (not Docker) for macOS TCP fingerprint
   - OR: Use a macOS-hosted TLS terminator as described above

3. **Add request rate limiting**
   - Minimum 2-3 second delay between requests to same account
   - Exponential backoff on 429 (not just account rotation)
   - Spread requests across accounts evenly

4. **Fix cross-layer consistency**
   - Match User-Agent to TLS fingerprint (Safari UA + Safari TLS)
   - Match HTTP/2 settings to browser profile
   - Ensure header ordering matches browser behavior

### Medium-Term (This Month)

5. **Evaluate tlsc or go-bypasser as uTLS replacement**
   - tlsc adds HTTP/2 fingerprint spoofing (uTLS doesn't)
   - go-bypasser adds User-Agent → TLS profile matching
   - Both provide built-in 429 retry with backoff

6. **Add traffic shaping**
   - Add random jitter to request timing (±10-30%)
   - Avoid predictable inter-request patterns
   - Consider multiplexing for streaming connections

7. **Implement region rotation**
   - Probe us-west-2 as alternative to us-east-1
   - Different regions may have different detection thresholds

### Long-Term

8. **Consider subprocess-based escalation**
   - Try Go uTLS → on persistent 429 → fallback to curl-impersonate subprocess → on further block → headless browser
   - The recurl escalation pattern is proven in production

9. **Monitor AWS WAF changes**
   - AWS continuously updates fingerprint blocklists
   - uTLS needs ongoing updates to stay ahead
   - Community-maintained fingerprint databases may be more current

---

## 7. Architecture Decision: Running on Mac vs Docker

| Factor | Docker (Linux VM) | Mac Native |
|--------|------------------|------------|
| TCP fingerprint | Linux (64240 window, Linux options) | macOS (65535 window, macOS options) |
| TLS fingerprint | uTLS (any) | uTLS (any) |
| Cross-layer consistency | Linux TCP + Safari TLS = **MISMATCH** | macOS TCP + Safari TLS = **CONSISTENT** |
| Deployment ease | ✅ Docker Compose | ⚠️ Manual binary/launchd |
| Security isolation | ✅ Containerized | ⚠️ Direct host access |
| curl-impersonate access | ❌ Cross-VM | ✅ Local subprocess |

**Recommendation**: Run the proxy **natively on Mac** with:
1. uTLS Safari fingerprint (already implemented)
2. Optional curl-impersonate sidecar for TLS termination
3. macOS TCP stack (free, no action needed — it's macOS!)

Or keep Docker but add a **Mac-side TLS-terminating proxy** so Docker traffic goes:
```
Docker → HTTP → Mac sidecar → curl-impersonate/SecureTransport TLS → Kiro
```

---

## 8. Key References

| Source | URL | Relevance |
|--------|-----|-----------|
| AWS WAF JA4 Announcement | https://aws.amazon.com/about-aws/whats-new/2025/03/aws-waf-ja4-fingerprinting/ | **Official confirmation** of JA4 detection |
| DoiT: JA3/JA4 in AWS WAF | https://www.doit.com/blog/ja3-and-ja4-fingerprints-in-aws-waf-and-beyond | Technical deep-dive on AWS fingerprinting |
| FoxIO: JA4T TCP Fingerprinting | https://foxio.io/blog/ja4t-tcp-fingerprinting | **Critical**: TCP-level detection |
| pydoll: Network Fingerprinting | https://pydoll.tech/docs/deep-dive/fingerprinting/network-fingerprinting/ | Per-OS TCP signatures |
| GoLogin: Device Fingerprinting 2026 | https://gologin.com/blog/device-fingerprinting/ | OS fingerprint comparison table |
| curl-impersonate | https://github.com/lwthiker/curl-impersonate | **Gold standard** for TLS anti-detection |
| uTLS Issue #321 | https://github.com/refraction-networking/utls/issues/321 | Known uTLS detection flaws |
| uTLS Architecture (DeepWiki) | https://deepwiki.com/refraction-networking/utls/1.1-features-and-architecture | uTLS limitations documented |
| tlsc: Go Full-Stack Spoofing | https://pkg.go.dev/github.com/Phrasing/tlsc | uTLS + HTTP/2 + HTTP/3 spoofing |
| go-bypasser | https://github.com/skycheung803/go-bypasser | Drop-in Go RoundTripper |
| mic: Modular Go Proxy | https://pkg.go.dev/github.com/djnnvx/mic | Reference Go proxy with uTLS |
| CLIProxyAPIPlus (Kiro Community) | https://github.com/ClubWeGo/CLIProxyAPIPlus | **Directly applicable**: Kiro-specific anti-detection |
| Kiro AWS Bypass Guide | https://www.80aj.com/2026/05/22/kiro-aws-bypass-guide/ | Documents the exact Kiro 429 problem |
| USENIX Security 2024 | https://www.usenix.org/system/files/usenixsecurity24-xue-fingerprinting.pdf | Academic: traffic analysis beats TLS spoofing |
| Scrapfly: TCP/IP Fingerprinting | https://scrapfly.io/blog/posts/tcp-ip-stack-fingerprinting-proxy-bypass | TCP fingerprinting in practice |
| CapMonster: TLS Anti-Bot Bypass | https://capmonster.cloud/blog/how-to-bypass-antibots-tls-fingerprinting-captcha-handling/ | 429 as response to suspicious fingerprints |
| recurl: Escalation-Layered Client | https://github.com/neul-labs/recurl | Subprocess escalation pattern |
| EvilWAF: WAF Bypass Tool | https://github.com/suryatmodulus/evilwaf | TCP/TLS fingerprint rotation |
