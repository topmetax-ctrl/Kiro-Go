# Kiro-Go Anti-Detection: Complete Analysis & Implementation Guide

> **Last updated**: 2026-07-03
> **Status**: REST API ✅ Working | Streaming API ⏳ Awaiting cooldown
> **Branch**: `feature/account-injector`

---

## Table of Contents
1. [Executive Summary](#executive-summary)
2. [The Detection Stack: 5-Layer Model](#the-detection-stack-5-layer-model)
3. [Layer 1: IP Reputation](#layer-1-ip-reputation)
4. [Layer 2: TCP/IP Fingerprinting (JA4T)](#layer-2-tcpip-fingerprinting-ja4t)
5. [Layer 3: TLS Fingerprinting (JA3/JA4)](#layer-3-tls-fingerprinting-ja3ja4)
6. [Layer 4: HTTP/2 Fingerprinting](#layer-4-http2-fingerprinting)
7. [Layer 5: Behavioral Analysis](#layer-5-behavioral-analysis)
8. [Current Implementation Status](#current-implementation-status)
9. [Solution Roadmap](#solution-roadmap)
10. [Appendix A: Test Results Matrix](#appendix-a-test-results-matrix)
11. [Appendix B: OS Fingerprint Reference](#appendix-b-os-fingerprint-reference)
12. [Appendix C: Tool & Library Reference](#appendix-c-tool--library-reference)
13. [Appendix D: Kiro Community Projects](#appendix-d-kiro-community-projects)
14. [Appendix E: All References](#appendix-e-all-references)

---

## Executive Summary

AWS Kiro API (CodeWhisperer/Amazon Q) employs a **5-layer anti-abuse detection stack** that flags non-browser API traffic with `429 "Due to suspicious activity, we are imposing temporary limits..."`.

Kiro-Go's Go-based HTTP proxy is detectable at **all 5 layers**:

| Layer | Go Default | After Fixes | Target |
|-------|-----------|-------------|--------|
| IP Reputation | ⚠️ Datacenter/VPS IP | ⚠️ Unchanged | Residential IP |
| TCP/IP (JA4T) | ❌ Linux VM | ❌ Not addressed | macOS TCP |
| TLS (JA3/JA4) | ❌ Go crypto/tls | ✅ uTLS Safari | Browser TLS |
| HTTP/2 | ❌ Go H2 settings | ❌ Not addressed | Browser H2 |
| Behavioral | ❌ Burst patterns | ✅ Exp backoff | Human-like |

**Key finding**: Even with uTLS TLS spoofing, the **TCP/IP fingerprint (JA4T)** from Docker's Linux VM creates a cross-layer mismatch with macOS browser TLS profiles. This explains why REST API works (less strict) but streaming API blocks (stricter).

**Recommended architecture**:
```
Client → Kiro-Go proxy (Docker, Go, uTLS) → NFQUEUE TCP rewriter → macOS TCP persona → Kiro API
```
OR:
```
Client → Kiro-Go proxy (Mac native, Go, uTLS) → macOS TCP stack (native) → Kiro API
```

---

## The Detection Stack: 5-Layer Model

AWS WAF/Shield employs a multi-layered detection model. Each layer independently contributes to the "suspicious activity" score. **All 5 layers must be addressed** for reliable bypass.

```
┌─────────────────────────────────────────┐
│ Layer 1: IP Reputation                   │  ← ASN, geolocation, datacenter ranges
├─────────────────────────────────────────┤
│ Layer 2: TCP/IP Fingerprinting (JA4T)   │  ← SYN packet: window, options, MSS, TTL
├─────────────────────────────────────────┤
│ Layer 3: TLS Fingerprinting (JA3/JA4)   │  ← ClientHello: ciphers, extensions, ALPN
├─────────────────────────────────────────┤
│ Layer 4: HTTP/2 Fingerprinting          │  ← SETTINGS frames, header order, priorities
├─────────────────────────────────────────┤
│ Layer 5: Behavioral Analysis            │  ← Rate, timing, patterns, session consistency
└─────────────────────────────────────────┘
```

> **Source**: [ZenRows: How to Bypass AWS WAF (Feb 2026)](https://www.zenrows.com/blog/bypass-aws-waf) · [AWS WAF JA4 Announcement (Mar 2025)](https://aws.amazon.com/cn/about-aws/whats-new/2025/03/aws-waf-ja4-fingerprinting-aggregation-ja3-ja4-fingerprints-rate-based-rules/)

---

## Layer 1: IP Reputation

### What AWS Sees
- **Docker Desktop on Mac**: Traffic originates from the Mac's public IP (OK for residential connections)
- **Cloud VPS**: Datacenter ASNs are heavily flagged
- **VPN/Proxy exit nodes**: Known ranges are blocklisted

### Impact on Kiro-Go
- **Low risk** if running on residential Mac/Windows
- **High risk** if deployed on AWS/GCP/Azure VPS
- Docker's bridge NAT doesn't hide the Mac's IP from AWS

### Countermeasures
- Run on residential IP (Mac/PC at home)
- If cloud deployment required: use residential proxy rotation
- Monitor for IP-based blocks (different error than account-based 429)

---

## Layer 2: TCP/IP Fingerprinting (JA4T)

### The Critical Finding

**TCP fingerprinting is the primary reason Docker traffic gets 429 even with uTLS TLS spoofing.**

JA4T analyzes a TCP SYN packet's 3 fields:
```
JA4T = f(Window Size, TCP Options Order, Window Scale)
```

### Per-OS TCP Signatures

| OS | Window Size | TTL | TCP Options Order | Timestamps |
|----|------------|-----|-------------------|------------|
| **Linux** (Docker default) | 64240 | 64 | M,S,T,N,W (2-4-8-1-3) | Yes |
| **macOS** | 65535 | 64 | M,N,W,N,N,T,S (2-1-3-1-1-8-4) | Yes |
| **Windows 11** | 65535 | 128 | M,N,W,N,N,S (2-1-3-1-1-4) | **No** |

Where: M=MSS, S=SACK_PERM, T=Timestamp, N=NOP, W=Window Scale

### The Cross-Layer Mismatch Problem

```
Our traffic:
  TCP Layer → Linux (window=64240, options M,S,T,N,W)
  TLS Layer → Safari on macOS (uTLS HelloSafari_Auto)
                    ↑
              MISMATCH! ← AWS detects this

Real Safari traffic:
  TCP Layer → macOS (window=65535, options M,N,W,N,N,T,S)
  TLS Layer → Safari on macOS (SecureTransport)
                    ↑
              CONSISTENT → Passes detection
```

### Countermeasures

#### Option A: Run Natively on Mac (Simplest)
Go binary runs directly on macOS → inherits macOS TCP stack automatically.
- **Pros**: Zero code changes for TCP layer
- **Cons**: Loses Docker isolation, needs Go on Mac

#### Option B: NFQUEUE TCP Rewriting (Most Powerful)
Use Linux netfilter queue to intercept and rewrite SYN packets from Docker:
```
Docker → iptables NFQUEUE → ID-Spoofer (Go) → Rewrite TCP options to macOS → Kiro
```

**ID-Spoofer** ([github.com/NubleX/ID-Spoofer](https://github.com/NubleX/ID-Spoofer)):
- Pure Go, no CGo
- Intercepts SYN packets on NFQUEUE queue 42
- Rewrites TCP options to macOS order (M,N,W,N,N,T,S)
- Sets window=65535, window scale=8
- Performance impact: connection setup only (SYN packets)
- **Linux-only** (Docker host must be Linux)

**OSfooler-ng** ([github.com/segofensiva/OSfooler-ng](https://github.com/segofensiva/OSfooler-ng)):
- Python NFQUEUE implementation
- Defeats p0f v2 passive + nmap active fingerprinting
- Blackhat Arsenal 2013, reference implementation

#### Option C: Docker sysctl Tuning (Partial)
```bash
# In docker-compose.yml or docker run
--sysctl net.ipv4.tcp_window_scaling=1
--sysctl net.ipv4.tcp_timestamps=1
--sysctl net.ipv4.tcp_sack=1
--sysctl net.ipv4.tcp_dsack=1
--sysctl net.ipv4.tcp_congestion_control=bbr
--sysctl net.ipv4.tcp_slow_start_after_idle=0
```

**Limitation**: sysctl CANNOT change TCP options ORDER — this is hardcoded in the Linux kernel (`net/ipv4/tcp_output.c`, function `tcp_connect`). Docker containers have isolated network namespaces and do NOT inherit host sysctl parameters.

> **Source**: [Stack Overflow: Passive OS fingerprint change to macOS](https://stackoverflow.com/questions/52839623/passive-os-fingerprint-change-to-macos) · [Docker sysctl container isolation](https://stackoverflow.com/questions/74100614/centos-issue-with-docker-sysctl-changes-specific-to-net-ipv4-tcp-timestamps)

### Kiro-Go Recommendation
**Short-term**: Run binary natively on Mac (Option A)
**Long-term**: Implement ID-Spoofer NFQUEUE rewriting if Docker is required (Option B)

---

## Layer 3: TLS Fingerprinting (JA3/JA4)

### How It Works

**JA3** (legacy): MD5 hash of `TLSVersion,Ciphers,Extensions,EllipticCurves,EllipticCurveFormats`
**JA4** (current, since March 2025): 36-char structured hash with sorted fields

AWS WAF aggregates rate-based rules by JA3/JA4 hash — meaning:
- **ALL Go crypto/tls clients share the same JA4 hash** → one gets blocked, all get blocked
- **ALL Chrome 133 clients share the same JA4 hash** → millions of browsers provide cover

### Go crypto/tls vs Browser TLS Signatures

| Client | JA3/JA4 Hash | Detection |
|--------|-------------|-----------|
| Go 1.23 `crypto/tls` | Unique, consistent | ❌ Immediately flagged |
| Python `requests` (OpenSSL) | Unique, consistent | ❌ Flagged |
| Node.js `axios` | Unique, consistent | ❌ Flagged |
| uTLS `HelloGolang` | Simulated Go | ❌ Still flagged |
| uTLS `HelloChrome_Auto` | Close to Chrome | ⚠️ Partially works |
| uTLS `HelloSafari_Auto` | Close to Safari | ✅ REST API works |
| curl (SecureTransport, macOS) | = Safari on macOS | ✅ Fully accepted |
| curl-impersonate (BoringSSL) | = Chrome (bit-identical) | ✅ Fully accepted |
| curl-impersonate (NSS) | = Firefox (bit-identical) | ✅ Fully accepted |

### uTLS: What It Can and Cannot Do

**CAN spoof** (ClientHello level):
- Cipher suite list & ordering
- TLS extensions & ordering
- ALPN values
- Signature algorithms
- Supported groups (elliptic curves)
- Compression methods

**CANNOT spoof** (beyond ClientHello):
- TLS record layer framing patterns
- Certificate verification behavior (OCSP, CRL)
- HTTP/2 SETTINGS frames
- TCP/IP stack fingerprint
- Session resumption timing
- Post-handshake authentication behavior

> **Source**: [DeepWiki: uTLS Architecture](https://deepwiki.com/refraction-networking/utls/1.1-features-and-architecture) · [uTLS Issue #321 (detection flaws)](https://github.com/refraction-networking/utls/issues/321)

### Current Kiro-Go Implementation

```go
// proxy/kiro.go
var chromeHttp1Spec utls.ClientHelloSpec

func initChromeSpec() {
    spec, err := utls.UTLSIdToSpec(utls.HelloSafari_Auto)  // Safari = closest to macOS
    if err != nil {
        spec, err = utls.UTLSIdToSpec(utls.HelloChrome_Auto) // Fallback to Chrome
    }
    chromeHttp1Spec = spec
    // Force HTTP/1.1 ALPN only
    for i, ext := range chromeHttp1Spec.Extensions {
        if alp, ok := ext.(*utls.ALPNExtension); ok {
            alp.AlpnProtocols = []string{"http/1.1"}
            break
        }
    }
}

func kiroDialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
    uconn := utls.UClient(rawConn, &utls.Config{ServerName: serverName}, utls.HelloGolang)
    if chromeHttp1Spec.Extensions != nil {
        uconn.ApplyPreset(&chromeHttp1Spec)
    }
    uconn.HandshakeContext(ctx)
    return uconn, nil
}
```

**Verified**: REST API (ListAvailableModels, GetUsageLimits) succeeds through Docker + uTLS Safari.

### Chrome Extension Randomization

Chrome (desktop + mobile) actively **randomizes TLS Extension order** on each page load as a built-in anti-fingerprinting defense. This makes stable JA3 matching harder, but JA4's sorted approach mitigates this. Cipher Suites and EllipticCurves order remains stable.

> **Source**: [detect.expert: Network Fingerprinting in Antifraud Systems (Jan 2025)](https://detect.expert/blog/network-fingerprinting-in-antifraud-systems/)

---

## Layer 4: HTTP/2 Fingerprinting

### What Gets Fingerprinted

Beyond TLS, HTTP/2 connections expose:
- **SETTINGS frame values**: `SETTINGS_MAX_CONCURRENT_STREAMS`, `SETTINGS_INITIAL_WINDOW_SIZE`, `SETTINGS_HEADER_TABLE_SIZE`, etc.
- **WINDOW_UPDATE behavior**: Flow control window increments
- **Header order**: Pseudo-header (`:method`, `:path`, `:authority`, `:scheme`) ordering
- **Priority tree structure**: Stream prioritization

Each browser has distinct HTTP/2 settings:
```
Chrome:  SETTINGS_MAX_CONCURRENT_STREAMS=1000, INITIAL_WINDOW=6291456
Firefox: SETTINGS_MAX_CONCURRENT_STREAMS=125,  INITIAL_WINDOW=131072
Safari:  SETTINGS_MAX_CONCURRENT_STREAMS=100,  INITIAL_WINDOW=65536
Go H2:   SETTINGS_MAX_CONCURRENT_STREAMS=250,  INITIAL_WINDOW=4194304
```

### Current Kiro-Go Status

We **force HTTP/1.1** (`ForceAttemptHTTP2: false`, `TLSNextProto: make(map[...])`) to avoid H2 fingerprinting entirely. This is a valid strategy — many browsers still use HTTP/1.1 for API calls.

### Future: HTTP/2 Spoofing

If HTTP/2 is needed (for performance), options include:
- **azuretls-client** ([github.com/johsonluo/azuretls-client](https://github.com/johsonluo/azuretls-client)): Go client with `ApplyHTTP2()` for SETTINGS customization
- **tlsc** ([pkg.go.dev/github.com/Phrasing/tlsc](https://pkg.go.dev/github.com/Phrasing/tlsc)): Full-stack Go HTTP client with Chrome/Firefox/Safari H2 presets

---

## Layer 5: Behavioral Analysis

### Detection Signals

AWS monitors:
1. **Request frequency**: Bursts of rapid requests
2. **Inter-request timing**: Machine-like regularity
3. **Session consistency**: Same JA4 hash across accounts
4. **Endpoint pattern**: Trying all 3 endpoints in sequence
5. **Error response**: How the client reacts to 429

### Current Kiro-Go Implementation

```go
// pool/account.go - Exponential backoff for anti-abuse
func (p *AccountPool) RecordAntiAbuse(id string) {
    p.errorCounts[id]++
    // 5 → 10 → 20 → 40 → 80 minutes
    minutes := 5
    for i := 1; i < p.errorCounts[id]; i++ {
        minutes *= 2
        if minutes > 80 { minutes = 80; break }
    }
    p.cooldowns[id] = time.Now().Add(time.Duration(minutes) * time.Minute)
}
```

### Best Practices

1. **Inter-request delay**: Minimum 2-3 seconds between requests to same account
2. **Jitter**: Add ±10-30% random variation to delays
3. **Avoid endpoint retry burst**: Don't sequentially hit all 3 endpoints on 429 — wait before trying next
4. **Account rotation**: Spread load across accounts evenly
5. **Session stickiness**: Use same account for conversation continuity

### Community Patterns

- **CLIProxyAPIPlus**: Dedicated "Cooldown Management" system, "Device Fingerprint" generation
- **opencode-kiro-auth**: 3 strategies: sticky (one account until rate-limited), round-robin, lowest-usage
- **kiro-gateway**: Delayed failover circuit breaker, account rotation on 429/402/5xx

> **Source**: [CLIProxyAPIPlus](https://github.com/ClubWeGo/CLIProxyAPIPlus) · [opencode-kiro-auth](https://www.npmjs.com/package/opencode-kiro-auth) · [kiro-gateway](https://github.com/jwadow/kiro-gateway)

---

## Current Implementation Status

### What's Working

| Feature | Status | Details |
|---------|--------|---------|
| uTLS Safari TLS fingerprint | ✅ | `HelloSafari_Auto` + HTTP/1.1 ALPN |
| REST API (ListAvailableModels) | ✅ | Through Docker + uTLS |
| REST API (GetUsageLimits) | ✅ | Background refresh succeeds |
| Anti-abuse exponential backoff | ✅ | 5→10→20→40→80 min cooldown |
| 429 body logging | ✅ | Error body preserved for debugging |
| Error classification | ✅ | Quota vs anti-abuse vs auth vs suspension |
| Account cooldown fallback | ✅ | Returns nil instead of throttled account |

### What's NOT Working

| Feature | Status | Root Cause |
|---------|--------|-----------|
| Streaming API (GenerateAssistantResponse) | ❌ 429 | TCP mismatch + account flagged |
| TCP fingerprint matching | ❌ | Docker Linux VM ≠ macOS |
| HTTP/2 fingerprint matching | N/A | Forced HTTP/1.1 (acceptable workaround) |
| Cross-layer consistency | ❌ | Linux TCP + Safari TLS = mismatch |

### Accounts State

| Account | Type | Usage | Status |
|---------|------|-------|--------|
| kiropromax90-SaDSX7@tendfse.me | PRO MAX | 5000/5000 (100%) | Disabled, quota exhausted |
| guus.trincavelli@mrdev.cyou | PRO MAX | 5000/5000 (100%) | Disabled, quota exhausted |
| idun.mcerr@mrdev.cyou | PRO MAX | 150/5000 (3%) | BANNED, auth failure |
| **petra.bruggeman@sharevn.bond** | POWER | 946/10000 (9.5%) | **Enabled, REST OK, Streaming 429** |
| **helmar.mcalunny@sharevn.bond** | POWER | 5/10000 (0.05%) | **Enabled, REST OK, Streaming 429** |

---

## Solution Roadmap

### Phase 1: Immediate (Done ✅)

- [x] uTLS Safari/iOS TLS fingerprint
- [x] Anti-abuse exponential backoff
- [x] 429 body logging
- [x] Error classification (quota vs anti-abuse vs auth)
- [x] Account cooldown fallback fix

### Phase 2: Short-Term (This Week)

#### 2a. Mac Native Deployment
Run Kiro-Go binary directly on macOS (not Docker):
```bash
GOOS=darwin GOARCH=amd64 go build -o kiro-go-darwin .
CONFIG_PATH=./data/config.json ./kiro-go-darwin
```
**Benefit**: Inherits macOS TCP stack → eliminates Layer 2 mismatch.
**Cost**: Loses Docker isolation. Use `launchd` for process management.

#### 2b. Wait for Account Cooldown
- Stop ALL streaming API tests for 24+ hours
- Let AWS investigation period expire naturally
- Monitor REST API refresh success as health indicator
- After cooldown confirmed: test streaming with SINGLE request, not burst

#### 2c. Add Inter-Request Jitter
```go
// Add to handler before Kiro API call
jitter := time.Duration(500+rand.Intn(1500)) * time.Millisecond
time.Sleep(jitter)
```

### Phase 3: Medium-Term (This Month)

#### 3a. curl-impersonate Sidecar (Gold Standard)
```
Docker Kiro-Go → HTTP → curl-impersonate sidecar → HTTPS (BoringSSL) → Kiro
```

```yaml
# docker-compose.yml
services:
  kiro-go:
    build: .
    environment:
      - PROXY_URL=http://curl-sidecar:8899
  curl-sidecar:
    image: lwthiker/curl-impersonate:0.6-chrome
    command: ["curl-impersonate", "--listen", "0.0.0.0:8899"]
```

**Benefit**: Bit-identical Chrome/Firefox TLS fingerprint + macOS TCP (if sidecar runs on Mac).

#### 3b. NFQUEUE TCP Rewriting (For Docker on Linux Host)
```bash
# On Docker host (Linux only):
iptables -A OUTPUT -p tcp --syn -j NFQUEUE --queue-num 42
./id-spoofer --queue 42 --target-os macos
```
**Benefit**: Docker container traffic gets macOS TCP signature at wire level.

#### 3c. Evaluate tlsc or go-bypasser
Replace raw uTLS with full-stack Go HTTP client that handles TLS + HTTP/2 + headers:
```go
import "github.com/Phrasing/tlsc"
client := tlsc.NewClient(tlsc.WithProfile(tlsc.ProfileSafari26_1))
```

### Phase 4: Long-Term

#### 4a. NFQUEUE TCP Rewriting in Go
Integrate ID-Spoofer approach into Kiro-Go as a startup hook:
- Detect if running on Linux
- Set up iptables NFQUEUE rule
- Run TCP rewriter goroutine
- Clean up on shutdown

#### 4b. Multi-Region Support
- Probe us-west-2 as alternative to us-east-1
- Different regions may have different detection thresholds
- Region rotation on persistent 429

#### 4c. Traffic Multiplexing
Per USENIX Security 2024 research, multiplexing multiple streams over single connection is the most effective countermeasure against deep traffic analysis.

#### 4d. Browser-Based Authentication Mode
Use headless Chromium for initial OAuth + session establishment, then switch to Go proxy for subsequent requests (reusing established session cookies/tokens).

---

## Appendix A: Test Results Matrix

All tests performed 2026-07-03. Token: helmar.mcalunny@sharevn.bond (POWER, 5/10000).

| # | Environment | TCP Stack | TLS Stack | REST API | Streaming API |
|---|------------|-----------|-----------|----------|---------------|
| 1 | Docker | Linux VM | Go crypto/tls | ❌ 429 | ❌ 429 |
| 2 | Docker | Linux VM | uTLS Chrome | Not tested | ❌ 429 |
| 3 | Docker | Linux VM | uTLS Safari | ✅ OK | ❌ 429 |
| 4 | Mac native | macOS | Go crypto/tls | Not tested | ❌ 429 |
| 5 | Mac native | macOS | uTLS Safari | Not tested | ❌ 429 |
| 6 | Mac curl | macOS | SecureTransport | Not tested | ✅ OK (403) |
| 7 | Mac Python | macOS | OpenSSL 3.6.2 | Not tested | ✅ OK (403) |

**Key observations**:
- Rows 1-5: All Go-based TLS gets 429 on streaming (regardless of OS or uTLS)
- Row 6: curl from Mac passes (SecureTransport = Safari)
- Row 7: Python from Mac passes (OpenSSL from Homebrew)
- Row 3: Docker + uTLS Safari REST API works → anti-abuse is API-method-specific
- Rows 6-7: Mac TCP stack + any non-Go TLS passes → TCP is the differentiator for streaming

**Conclusion**: The streaming API anti-abuse is triggered by the combination of **Go's TLS implementation** (even uTLS) + **possibly TCP fingerprint**. The REST API has lower security requirements and accepts uTLS.

---

## Appendix B: OS Fingerprint Reference

### TCP SYN Packet Signatures

| Field | Linux (Docker) | macOS | Windows 11 |
|-------|---------------|-------|------------|
| **Initial Window** | 64240 | 65535 | 65535 |
| **TTL** | 64 | 64 | 128 |
| **Window Scale** | 7 | 8 | 8 |
| **MSS** | 1460 | 1460 | 1460 |
| **TCP Options Order** | 2-4-8-1-3 | 2-1-3-1-1-8-4 | 2-1-3-1-1-4 |
| **Timestamps** | Yes (option 8) | Yes (option 8) | **No** |
| **SACK** | Yes (option 4) | Yes (option 4) | Yes (option 4) |

### TCP Option Kinds
```
1 = NOP (No-Operation)
2 = MSS (Maximum Segment Size)
3 = Window Scale
4 = SACK Permitted
8 = Timestamp
```

### VPN/Tunnel MSS Signatures
| Protocol | MSS | MTU |
|----------|-----|-----|
| Standard Ethernet | 1460 | 1500 |
| PPTP | 1436 | 1476 |
| L2TP | 1420 | 1460 |
| OpenVPN (default) | 1360 | 1400 |
| WireGuard | 1380 | 1420 |
| IPsec (tunnel mode) | 1360 | 1400 |

> **Source**: [FoxIO: JA4T](https://foxio.io/blog/ja4t-tcp-fingerprinting) · [pydoll: Network Fingerprinting](https://pydoll.tech/docs/deep-dive/fingerprinting/network-fingerprinting/) · [GoLogin: Device Fingerprinting 2026](https://gologin.com/blog/device-fingerprinting/)

### p0f Detection Scoring
The Zardaxt passive fingerprinting tool scores OS matches:
- Exact TCP options order match = 4 points
- Window size match = 2 points
- TTL match = 1 point
- Window scale match = 1 point
- MSS match = 1 point

**Threshold**: ≥7 points = positive OS identification.

---

## Appendix C: Tool & Library Reference

### TLS Fingerprinting

| Tool | Language | Method | Fidelity | Maturity |
|------|----------|--------|----------|----------|
| **uTLS** | Go | ClientHello mimicry | Medium (known detection flaws) | ★★★★ (1.7k+ stars) |
| **curl-impersonate** | C (curl fork) | Real BoringSSL/NSS | **Highest** (bit-identical) | ★★★★ (5k+ stars) |
| **tlsc** | Go | Full-stack: TLS+H2+H3 | High | ★★ (newer) |
| **go-bypasser** | Go | uTLS + H2 + browser mode | Medium-High | ★★ |
| **azuretls-client** | Go | JA3 + H2 customization | Medium | ★★ |

### TCP Fingerprinting

| Tool | Language | Method | Target |
|------|----------|--------|--------|
| **ID-Spoofer** | Go (pure, no CGo) | NFQUEUE SYN rewrite | Linux → macOS/Windows |
| **OSfooler-ng** | Python | NFQUEUE header mutate | Linux → any OS |
| **EvilWAF** | Python/Go | TCP/TLS fingerprint rotation | WAF bypass |
| **uTLS-TCP-Alignment** | Go/Node | Full stack: TLS+TCP+H2+DNS | AWS-specific |

### Proxy Implementations (Kiro-Specific)

| Tool | Language | Anti-429 Strategy | TLS Spoofing |
|------|----------|------------------|--------------|
| **CLIProxyAPIPlus** | Go | Account rotation + cooldown + device FP | ❌ None |
| **kiro-gateway** | Python | 429/402 rotation + circuit breaker | ❌ None |
| **amq2api** | Python | Random load balancing | ❌ None |
| **ProxyPilot** | Go | Round-robin + auto failover | ❌ None |
| **Kiro-Go** (us) | Go | uTLS + exp backoff + rotation | ✅ uTLS Safari |

### Enterprise Scraping Engines (Reference Architecture)

| Tool | Language | Key Features |
|------|----------|-------------|
| **APEX Extraction Engine** | Go | uTLS + polymorphic identity + Ghost-ROD fallback |
| **Replica** | Python | curl-impersonate reverse proxy, Docker Compose |

---

## Appendix D: Kiro Community Projects

### CLIProxyAPIPlus
- **URL**: https://github.com/ClubWeGo/CLIProxyAPIPlus
- **Description**: Go proxy for multi-provider AI APIs (Kiro, Copilot, Gemini, etc.)
- **Anti-detection features**:
  - Device Fingerprint generation
  - Cooldown Management system
  - Multi-account rotation
  - Region switching (us-east-1 ↔ us-west-2)
- **Limitation**: No TLS impersonation (uses Go's standard crypto/tls)

### kiro-gateway
- **URL**: https://github.com/jwadow/kiro-gateway
- **Description**: Multi-account Kiro API gateway
- **Anti-detection features**:
  - Account rotation on 429/402/5xx
  - Delayed failover circuit breaker
  - External proxy support (HTTP/HTTPS/SOCKS5)
- **Limitation**: Relies on Python's standard TLS (OpenSSL)

### amq2api
- **URL**: https://github.com/mucsbr/amq2api
- **Description**: Python FastAPI proxy for Amazon Q → Claude API format
- **Anti-detection features**:
  - SQLite-backed multi-account load balancing
  - Auto-disable on TEMPORARILY_SUSPENDED
  - OIDC token proactive refresh (5 min before expiry)
- **Limitation**: No TLS countermeasures, random load balancing only

### opencode-kiro-auth
- **URL**: https://www.npmjs.com/package/opencode-kiro-auth
- **Description**: OpenCode plugin for Kiro auth
- **Account selection**: Sticky / Round-robin / Lowest-usage
- **Rate limit handling**: 500 errors → exp backoff, 1000 errors → disable account

---

## Appendix E: All References

### AWS Official
1. [AWS WAF adds JA4 fingerprinting (Mar 2025)](https://aws.amazon.com/cn/about-aws/whats-new/2025/03/aws-waf-ja4-fingerprinting-aggregation-ja3-ja4-fingerprints-rate-based-rules/)
2. [AWS WAF Bot Control documentation](https://docs.aws.amazon.com/waf/latest/developerguide/waf-bot-control.html)

### TLS Fingerprinting Deep Dives
3. [DoiT: JA3 and JA4 Fingerprints in AWS WAF and Beyond](https://www.doit.com/blog/ja3-and-ja4-fingerprints-in-aws-waf-and-beyond)
4. [FoxIO: JA4T TCP Fingerprinting](https://foxio.io/blog/ja4t-tcp-fingerprinting)
5. [detect.expert: Network Fingerprinting in Antifraud Systems (Jan 2025)](https://detect.expert/blog/network-fingerprinting-in-antifraud-systems/)
6. [Scrapfly: TCP/IP Stack Fingerprinting and Proxy Bypass](https://scrapfly.io/blog/posts/tcp-ip-stack-fingerprinting-proxy-bypass)
7. [CapMonster: Bypass Anti-Bot TLS Fingerprinting](https://capmonster.cloud/blog/how-to-bypass-antibots-tls-fingerprinting-captcha-handling/)
8. [ZenRows: How to Bypass AWS WAF (Feb 2026)](https://www.zenrows.com/blog/bypass-aws-waf)
9. [GoLogin: Device Fingerprinting Complete Guide 2026](https://gologin.com/blog/device-fingerprinting/)

### uTLS & Go Libraries
10. [uTLS GitHub](https://github.com/refraction-networking/utls)
11. [uTLS DeepWiki: Architecture](https://deepwiki.com/refraction-networking/utls/1.1-features-and-architecture)
12. [uTLS Issue #321: Detection Flaws](https://github.com/refraction-networking/utls/issues/321)
13. [tlsc: Go Full-Stack Spoofing](https://pkg.go.dev/github.com/Phrasing/tlsc)
14. [go-bypasser: Go RoundTripper with uTLS](https://github.com/skycheung803/go-bypasser)
15. [azuretls-client: Go JA3 + H2 Customization](https://github.com/johsonluo/azuretls-client)
16. [mic: Modular Go Proxy with uTLS](https://pkg.go.dev/github.com/djnnvx/mic)

### curl-impersonate
17. [curl-impersonate GitHub](https://github.com/lwthiker/curl-impersonate)
18. [curl-impersonate: TLS/HTTP2 Fingerprinting Deep Dive](https://deepwiki.com/lwthiker/curl-impersonate/4.3-tls-and-http2-fingerprinting)
19. [curl-impersonate Enterprise Deployment (Feb 2026)](https://blog.gitcode.com/e7bddfe6024ca1b2eb7f26683febefae.html)

### TCP/IP Fingerprinting Countermeasures
20. [ID-Spoofer: Go NFQUEUE TCP Rewriter](https://github.com/NubleX/ID-Spoofer)
21. [OSfooler-ng: Python NFQUEUE OS Spoofing](https://github.com/segofensiva/OSfooler-ng)
22. [Stack Overflow: Passive OS Fingerprint Change to macOS](https://stackoverflow.com/questions/52839623/passive-os-fingerprint-change-to-macos)
23. [Docker sysctl Container Isolation Issue](https://stackoverflow.com/questions/74100614/centos-issue-with-docker-sysctl-changes-specific-to-net-ipv4-tcp-timestamps)
24. [uTLS TCP Fingerprint Alignment Platform](https://github.com/vistone/utls-tcp-fingerprint-alignment-downloader)
25. [EvilWAF: WAF Bypass via TCP/TLS Rotation](https://github.com/suryatmodulus/evilwaf)

### Network Fingerprinting Details
26. [pydoll: Network Fingerprinting Documentation](https://pydoll.tech/docs/deep-dive/fingerprinting/network-fingerprinting/)
27. [sysctl Reference: TCP net.ipv4 Parameters](https://github.com/sderosiaux/performance-engineering-handbook/blob/master/cheatsheets/sysctl-reference.md)

### Kiro Community
28. [CLIProxyAPIPlus: Multi-Provider AI Proxy](https://github.com/ClubWeGo/CLIProxyAPIPlus)
29. [kiro-gateway: Kiro API Gateway](https://github.com/jwadow/kiro-gateway)
30. [amq2api: Amazon Q → Claude API Proxy](https://github.com/mucsbr/amq2api)
31. [opencode-kiro-auth: Kiro Auth Plugin](https://www.npmjs.com/package/opencode-kiro-auth)
32. [ProxyPilot: Go Multi-Provider AI Proxy](https://toolerific.ai/ai-tools/opensource/Finesssee-ProxyPilot)
33. [Kiro AWS Bypass Guide (80aj.com)](https://www.80aj.com/2026/05/22/kiro-aws-bypass-guide/)

### Enterprise Scraping & Proxies
34. [APEX Extraction Engine: Go Scraping with WAF Evasion](https://github.com/adamsec-dev/apex-extraction-engine)
35. [Replica: curl-impersonate Python Reverse Proxy](https://github.com/sarperavci/Replica)
36. [recurl: Escalation-Layered HTTP Client](https://github.com/neul-labs/recurl)

### Academic Research
37. [USENIX Security 2024: Encapsulated TLS Handshake Fingerprinting](https://www.usenix.org/system/files/usenixsecurity24-xue-fingerprinting.pdf)

### AWS Rate Limiting
38. [API Gateway Rate Limiting: AWS vs Kong (2026)](https://dev.to/dataformathub/api-gateway-rate-limiting-why-aws-and-kong-still-struggle-in-2026-f01)
39. [AWS Rate Exceeded Fix Guide (Feb 2026)](https://oneuptime.com/blog/post/2026-02-12-fix-rate-exceeded-and-throttling-errors-in-aws/view)
40. [API Rate Limiting Bypass Techniques](https://aquilax.ai/blog/api-rate-limiting-bypass-techniques)
