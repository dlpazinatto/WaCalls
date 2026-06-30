# WaCalls Asterisk Gateway

This fork adds a SIP/RTP gateway between WhatsApp Voice and Asterisk.

The browser/WebRTC UI from WaCalls still works, but the gateway can now bridge calls directly:

```text
WhatsApp caller
  <-> WhatsApp Voice / relay
WaCalls
  <-> SIP + RTP PCMU/8000
Asterisk
  <-> SIP/WebRTC agents, queues, IVR, recording, transfers
```

DTMF is intentionally out of scope for now. WhatsApp Voice does not expose an in-call dial pad, so there is no native DTMF event to forward from WhatsApp to Asterisk.

## Runtime Configuration

Use `.env` with Docker Compose.

```env
WACALLS_HTTP_ADDR=:8080
WACALLS_HTTP_PORT=8080
WACALLS_DB_PATH=/data/wacalls.db
WACALLS_STATIC_DIR=/app/client/dist
WACALLS_MAX_CALLS_PER_SESSION=8

WACALLS_ADVERTISE_IP=177.153.59.77
WACALLS_ASTERISK_SIP_LISTEN=:25419
WACALLS_ASTERISK_SIP_PORT=25419
WACALLS_ASTERISK_UAC_SIP_BIND=:0
WACALLS_RTP_MIN=40000
WACALLS_RTP_MAX=40100

WACALLS_ASTERISK_SIP_SERVER=
WACALLS_ASTERISK_SIP_FROM=wacalls
```

| Variable | Purpose |
|---|---|
| `WACALLS_ADVERTISE_IP` | IP advertised by WaCalls in SIP Contact and SDP. On a VPS, use the public IP. |
| `WACALLS_ASTERISK_SIP_LISTEN` | Local UDP listen address for SIP requests from Asterisk to WaCalls. |
| `WACALLS_ASTERISK_SIP_PORT` | Host/container UDP port published by Docker Compose for `WACALLS_ASTERISK_SIP_LISTEN`. |
| `WACALLS_ASTERISK_UAC_SIP_BIND` | Local UDP bind for WaCalls-originated SIP client legs. Keep `:0` so the OS chooses an ephemeral source port. |
| `WACALLS_RTP_MIN` / `WACALLS_RTP_MAX` | UDP RTP port pool used by both SIP directions. Publish/open this range. |
| `WACALLS_ASTERISK_SIP_SERVER` | Optional legacy fallback for WhatsApp -> Asterisk. Prefer per-session routes. |
| `WACALLS_ASTERISK_SIP_FROM` | Fallback SIP user for legacy calls. The caller ID normally comes from the WhatsApp caller. |

Open these UDP ports on the WaCalls host:

- SIP listen port, for example `25419/udp`
- RTP range, for example `40000-40100/udp`

WaCalls also needs outbound UDP access to WhatsApp relays and to the Asterisk SIP/RTP host.

## SQLite Data

The container uses:

```yaml
volumes:
  - ./data:/data
```

The SQLite database path is:

```text
/data/wacalls.db
```

To move an existing paired session to another host, stop the old container first and copy:

```text
./data/wacalls.db
```

Do not run two WaCalls instances with the same WhatsApp session at the same time.

## Route API

Routes are stored in SQLite table `asterisk_routes`.

List paired WhatsApp sessions:

```bash
curl http://127.0.0.1:8080/api/asterisk/sessions
```

List routes:

```bash
curl http://127.0.0.1:8080/api/asterisk/routes
```

Create a route:

```bash
curl -X POST http://127.0.0.1:8080/api/asterisk/routes \
  -H 'Content-Type: application/json' \
  -d '{
    "sessionId": "5a5f7a80af9e426eebcdab30152d2ea4",
    "waNumber": "554988046533",
    "sipServer": "sip001.dlp360.com:25417",
    "enabled": true
  }'
```

Update a route:

```bash
curl -X PUT http://127.0.0.1:8080/api/asterisk/routes/ROUTE_ID \
  -H 'Content-Type: application/json' \
  -d '{
    "sessionId": "5a5f7a80af9e426eebcdab30152d2ea4",
    "waNumber": "554988046533",
    "sipServer": "sip001.dlp360.com:25417",
    "enabled": true
  }'
```

Delete a route:

```bash
curl -X DELETE http://127.0.0.1:8080/api/asterisk/routes/ROUTE_ID
```

Fields:

| Field | Required | Purpose |
|---|---:|---|
| `sessionId` | yes | WaCalls session that owns the paired WhatsApp account. |
| `waNumber` | yes | Paired WhatsApp number used by this route, digits only. |
| `sipServer` | yes | Asterisk host and SIP port, IP or FQDN. Example: `sip001.dlp360.com:25417`. |
| `sipFrom` | no | Legacy fallback SIP from user. |
| `toUser` | no | Optional override for SIP Request-URI/To user in WhatsApp -> Asterisk. |
| `fromUser` | no | Optional override for SIP From/P-Asserted-Identity in WhatsApp -> Asterisk. |
| `enabled` | yes | Enables or disables the route. |

`sipTarget` is still accepted as a backward-compatible alias for `sipServer`.

## WhatsApp -> WaCalls -> Asterisk

When a WhatsApp call arrives:

1. WaCalls receives the WhatsApp call offer.
2. WaCalls resolves the WhatsApp caller from LID to PN when possible.
3. WaCalls finds an enabled route by `sessionId` or `waNumber`.
4. WaCalls sends a SIP INVITE to `sipServer`.
5. When Asterisk answers with `200 OK`, WaCalls accepts the WhatsApp call.
6. RTP PCMU/8000 is bridged to/from WhatsApp audio.

Caller/callee defaults:

- SIP `From`, `P-Asserted-Identity`, `Remote-Party-ID`: WhatsApp caller number.
- SIP Request-URI and `To`: paired WhatsApp number that received the call.

Per-route `fromUser` and `toUser` can override those values when needed.

Expected log lines:

```text
incoming call ... peer=...
resolved LID caller to PN ...
asterisk SIP INVITE sent ... remote_sip=... rtp_port=...
asterisk SIP leg answered ... rtp=...
call accepted
relay connected -> active
```

Hangup behavior:

- Asterisk sends `BYE`: WaCalls ends the WhatsApp call.
- WhatsApp ends the call: WaCalls sends `BYE` to Asterisk.

## Asterisk -> WaCalls -> WhatsApp

Asterisk sends an INVITE to the WaCalls SIP listen port:

```text
INVITE sip:<whatsapp_destination>@<wacalls_host>:25419 SIP/2.0
X-WaCalls-Number: <paired_whatsapp_origin>
```

Example:

```text
INVITE sip:554984029393@177.153.59.77:25419 SIP/2.0
X-WaCalls-Number: 554988046533
```

Rules:

- `X-WaCalls-Number` is required.
- The header value must match `waNumber` on an enabled route.
- The source IP of the INVITE must match the IP resolved from that same route's `sipServer`.
- The route's `sessionId` must be paired and its actual WhatsApp number must match `X-WaCalls-Number`.
- There is no automatic fallback by IP or by single session.

On success:

1. WaCalls responds `100 Trying`.
2. WaCalls originates the WhatsApp call using the session from the matched route.
3. WaCalls sends `180 Ringing` to Asterisk.
4. When the WhatsApp destination answers, WaCalls sends `200 OK` with SDP.
5. RTP PCMU/8000 is bridged to/from WhatsApp audio.

Expected log lines:

```text
asterisk SIP INVITE received ... wa_number=... target=...
outbound WhatsApp call started from Asterisk ... sip_call_id=... call_id=...
remote accepted call ...
asterisk outbound SIP leg answered ... rtp=...
call ACTIVE (media path established)
```

Reject behavior:

- Missing `X-WaCalls-Number`: `400 X-WaCalls-Number Required`.
- Unknown source IP or number/route mismatch: `403 Forbidden`.
- Route exists but session is not paired or number mismatches: `404 No WhatsApp Session`.

Hangup behavior:

- Asterisk sends `BYE`: WaCalls ends the WhatsApp call.
- Asterisk sends `CANCEL` before answer: WaCalls cancels the WhatsApp call.
- WhatsApp ends the call: WaCalls sends `BYE` to Asterisk.
- SIP retransmitted INVITEs with the same Call-ID are idempotent and do not start duplicate WhatsApp calls.

## PJSIP Examples

For a WaCalls host at `177.153.59.77:25419`, identify by IP only. Do not include WaCalls' listen port in the `identify` match, because WaCalls-originated INVITEs may use an ephemeral source port.

```ini
[trunk1_wacalls_identify]
type=identify
endpoint=trunk1_wacalls
match=177.153.59.77/32
```

Recommended endpoint options:

```ini
[trunk1_wacalls]
type=endpoint
transport=transport-udp
context=from-wacalls
disallow=all
allow=ulaw
direct_media=no
rtp_symmetric=yes
force_rport=yes
rewrite_contact=yes
```

For Asterisk behind NAT, configure the transport to advertise public signaling/media:

```ini
[transport-udp]
type=transport
protocol=udp
bind=0.0.0.0:25417
external_signaling_address=sip001.dlp360.com
external_signaling_port=25417
external_media_address=sip001.dlp360.com
local_net=192.168.80.0/24
```

Open/forward:

- SIP UDP port, for example `25417/udp`
- RTP UDP range, for example `15000-16000/udp`

If the `200 OK` SDP advertises an internal IP such as `192.168.80.200`, the VPS will send RTP to an unreachable address. The SDP must advertise the public IP/FQDN-resolved address:

```text
c=IN IP4 <PUBLIC_ASTERISK_IP>
m=audio 15008 RTP/AVP 0
```

## AEL Header Injection

`PJSIP_HEADER(add,...)` must run on the outgoing PJSIP channel. In AEL, use a `Dial()` `b()` subroutine:

```asterisk
context add_wacalls_header {
  s => {
    NoOp("Adding X-WaCalls-Number ${ARG1}");
    Set(PJSIP_HEADER(add,X-WaCalls-Number)=${ARG1});
    Return();
  };
};

context saida_wacall {
  _X. => {
    Set(WACALLS_NUMBER=554988046533);
    Dial(PJSIP/${DSTORIGINAL}@${TRONCO},60,b(add_wacalls_header^s^1(${WACALLS_NUMBER}))TWK);
    HangUp();
  };
};
```

## Code Map

The gateway code is intentionally kept in `cmd/server` so the upstream WaCalls code remains easy to compare.

| File | Responsibility |
|---|---|
| `asterisk_gateway.go` | Shared gateway defaults, RTP port pool, route/source validation helpers. |
| `asterisk_routes_store.go` | SQLite route table and migration helpers for `asterisk_routes`. |
| `asterisk_httpapi.go` | HTTP API for paired sessions and Asterisk route CRUD. |
| `sip_leg.go` | SIP client/UAC leg for WhatsApp -> Asterisk calls. Sends INVITE to Asterisk and bridges RTP PCMU. |
| `sip_uas.go` | SIP server/UAS leg for Asterisk -> WhatsApp calls. Receives INVITE/OPTIONS/CANCEL/BYE and bridges RTP PCMU. |
| `session.go` | Wires WhatsApp call events to SIP legs and routes peer audio to/from local media legs. |
| `callregistry.go` | Tracks active calls and their current local media leg. |
| `main.go` / `server.go` | Load env/flags, initialize route store and start the SIP listener. |

Media assumptions:

- SIP/RTP uses PCMU/8000, payload type `0`.
- Audio is resampled between 8 kHz SIP and 16 kHz WhatsApp PCM.
- DTMF/telephone-event is not implemented.

Important methods:

- `Session.bridgeIncomingToAsterisk`: WhatsApp inbound call -> SIP INVITE to Asterisk.
- `AsteriskSIPServer.handleInvite`: Asterisk INVITE -> WhatsApp outbound call.
- `SIPLeg.WritePCM` and `SIPInboundLeg.WritePCM`: WhatsApp audio -> RTP.
- `SIPLeg.rtpLoop` and `SIPInboundLeg.rtpLoop`: RTP -> WhatsApp audio.
- `AsteriskGateway.outboundRouteForSource`: validates `X-WaCalls-Number` against Asterisk source IP and route.

## Troubleshooting

### PJSIP says `No matching endpoint found`

Check `identify`. Match only the WaCalls IP:

```ini
match=177.153.59.77/32
```

Do not match `177.153.59.77:25419/32`, because WaCalls can send SIP from an ephemeral source port.

### PJSIP qualifies but calls are rejected

Check the INVITE has:

```text
X-WaCalls-Number: <paired_whatsapp_origin>
```

Also confirm `/api/asterisk/routes` has an enabled route where:

- `waNumber` equals that header.
- `sipServer` resolves to the IP from `remote_sip` in WaCalls logs.

### No RTP from WaCalls to Asterisk

Inspect the Asterisk `200 OK` SDP. If it advertises an internal address:

```text
c=IN IP4 192.168.x.x
```

configure PJSIP `external_media_address`, `external_signaling_address`, and `local_net`.

### Duplicate INVITEs in packet capture

If Call-ID, CSeq, and branch are identical and the packets are microseconds apart, it is often a capture artifact across host/Docker bridge or NAT. WaCalls handles retransmitted INVITEs idempotently.

Confirm with:

```asterisk
pjsip set logger on
```
