# dotmix

Polkadot SDK (Substrate) JSON-RPC through the [Nym mixnet](https://nym.com): Polkadot, Polkadot Asset Hub, Kusama,
Kusama Asset Hub, Bittensor, and the Paseo and Westend testnets, or any other Substrate chain you add. One Go binary,
no dependencies outside the standard library, MIT.

```
dotmix nym setup                       # starts nym-socks5-client on a random active exit gateway
dotmix serve                           # local gateway: ws://127.0.0.1:9944 (polkadot), /assethub, /kusama, ...
dotmix info --chain assethub           # chain, runtime, best and finalized block
dotmix balance 13UVJyLnbV...           # DOT/KSM free, reserved, frozen, nonce; USDT and USDC on Asset Hub
dotmix rpc chain_getBlockHash 1        # any one-shot JSON-RPC call
dotmix submit xt.hex --chain assethub  # forward an already signed extrinsic; dotmix holds no keys
dotmix probe                           # which public nodes answer Nym exit traffic, and how fast
dotmix ip                              # the IP the nodes see (a Nym exit gateway)
```

## Why

A Polkadot wallet keeps a websocket open to an RPC node and asks it about your accounts all the time: balances and
nonces (`state_getStorage` / `state_queryStorageAt` with keys that contain your account id), storage subscriptions
for every account in the wallet, fee estimates, and finally `author_submitExtrinsic`. The node operator sees all of
that next to your IP, for as long as the wallet is open. So it learns which IP holds which accounts, and which IP
sent which extrinsic. Most users never change the default endpoint, so a few operators see most of the traffic.

polkadot.js apps, Talisman, SubWallet, the polkadot{.js} extension, `@polkadot/api`, PAPI and subxt all let you set
a custom RPC endpoint, but none of them has a proxy setting. `dotmix serve` gives you a plain `ws://127.0.0.1:9944`
for that field. The node then sees a Nym exit gateway, not you.

## Use

```
export DOTMIX_NYM_SOCKS5=/path/to/nym-socks5-client    # official Nym release binary
go install github.com/apeirs/dotmix/cmd/dotmix@latest
dotmix nym setup            # SOCKS5 on 127.0.0.1:1089 (--port to change; then set DOTMIX_PROXY)
dotmix serve &
```

The gateway serves every chain on one port:

| Chain | Endpoint | Upstreams (tried in order) |
|---|---|---|
| Polkadot (the `--default`) | `ws://127.0.0.1:9944` or `/polkadot` | rpc.polkadot.io, polkadot-rpc.publicnode.com, rpc-polkadot.luckyfriday.io, polkadot.api.onfinality.io |
| Polkadot Asset Hub | `ws://127.0.0.1:9944/assethub` | polkadot-asset-hub-rpc.polkadot.io, rpc-asset-hub-polkadot.luckyfriday.io, statemint.api.onfinality.io |
| Kusama | `/kusama` | kusama-rpc.polkadot.io, kusama-rpc.publicnode.com, rpc-kusama.luckyfriday.io, kusama.api.onfinality.io |
| Kusama Asset Hub | `/kusama-assethub` | kusama-asset-hub-rpc.polkadot.io, rpc-asset-hub-kusama.luckyfriday.io |
| Bittensor | `/bittensor` | entrypoint-finney.opentensor.ai, lite.chain.opentensor.ai, bittensor-finney.api.onfinality.io |
| Paseo, Westend, Westend Asset Hub (testnets) | `/paseo`, `/westend`, `/westend-assethub` | stakeworld, dwellir; Parity |
| Your own node or chain | `/<name>` after `--add name=wss://node\|wss://node2` | yours |

Each endpoint speaks websocket JSON-RPC (subscriptions included) and JSON-RPC over HTTP POST (`http://127.0.0.1:9944/assethub`).
Requests and answers pass through byte for byte. Since the 2025 Asset Hub migration, DOT and KSM balances,
transfers and USDT/USDC live on the Asset Hub chains, so point a wallet at `/assethub` for those.

Put the URL into your client:

- **@polkadot/api** (tested with 16.5.6): `new WsProvider('ws://127.0.0.1:9944/assethub')`. Connect (metadata
  included), `query.system.account`, `query.assets.account`, `rpc.chain.subscribeNewHeads` and a raw
  `author_submitExtrinsic` all went through the gateway (`examples/polkadotjs-client.mjs`).
- **polkadot.js apps** (tested in a headless Chromium): start the gateway with
  `--allow-origin https://polkadot.js.org`, then open `https://polkadot.js.org/apps/?rpc=ws%3A%2F%2F127.0.0.1%3A9944%2Fassethub`.
  The explorer loaded and followed new blocks over the mixnet (`examples/polkadotjs-apps-explorer.png`). The page
  itself still comes from polkadot.js.org over your normal connection; only its RPC goes through Nym. It also
  loaded Google Fonts directly.
- **Talisman, SubWallet, polkadot{.js} extension**: add a custom RPC endpoint `ws://127.0.0.1:9944/assethub`.
  Requests from browser extensions are allowed without a flag. I have not tested these extensions yet.
- **PAPI, subxt, curl**: any tool that takes a `ws://` or `http://` RPC URL. curl:
  `curl -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"system_chain","params":[]}' http://127.0.0.1:9944/kusama`.

Watch the gateway log when you wire up a new app. It prints one line per websocket, with the methods that went
through it (names only), and one per POST. Anything that doesn't show up there did not go through Nym.

dotmix forwards; it never signs anything. `dotmix submit` takes an extrinsic that is already signed (hex, a file or
`-` for stdin), checks that its SCALE length prefix matches and that it is signed (`--allow-unsigned` to override),
and calls `author_submitExtrinsic`. `dotmix balance` decodes `System.Account` itself (and `Assets.Account` for USDT
1984 and USDC 1337 on Asset Hub) from an SS58 address of any prefix. Without `--chain` it asks every mainnet chain
that uses the address's prefix (`1...` -> Polkadot and Polkadot Asset Hub, Kusama-style addresses -> Kusama and its
Asset Hub).

## How it uses the mixnet

- It drives the official `nym-socks5-client` and does no mixing itself. `nym setup` picks a random active exit
  gateway from the Nym API (one that runs an exit-policy network requester), with reply SURBs on, so the exit
  doesn't learn your mixnet address.
- Each upstream connection is a SOCKS5 stream through the entry gateway, three mix layers and the exit, to port
  443. TLS runs end to end with the node, so the exit only relays ciphertext. A websocket stays open on its stream,
  so subscriptions and later calls don't pay a new handshake.
- Node hostnames are resolved by DNS-over-HTTPS *through the mixnet*, then connected by IPv4. The local resolver
  never learns which node you use.
- **Genesis guard.** Before a websocket is handed to the wallet, and before the first POST to a node, dotmix asks
  the node for its block-0 hash and compares it with the chain's. A node that serves another chain (a typo in
  `--add`, a misconfigured endpoint, a node that changed hands) is skipped and the next one is tried; your requests
  never reach it. Built-in chains have their genesis pinned in the code. A chain you add pins the genesis of its
  first node, and every other node of it has to match.
- The gateway can only reach the configured nodes. The path picks the chain (`/` or `/<chain>`) and nothing else:
  no part of it is forwarded, so a caller can't steer a request to another URL. Encoded paths are refused.
- There is no clearnet fallback. With the mixnet client down, the gateway answers 502 with a JSON-RPC error
  (`"data":"MixnetUnavailable"`) and the CLI refuses to run.
- Failover: a connection error, a timeout, a failed genesis check, HTTP 5xx, 429 or a redirect moves on to the next
  node. Redirects are never followed or passed back.
- Nothing from the local caller is forwarded except the JSON-RPC itself. Upstream sees only the websocket handshake
  headers, or `Content-Type` and `Accept: application/json`, plus a fixed `User-Agent`: no `Origin`, `Cookie`, `Authorization`,
  `Referer` or the wallet's own user agent. Only `Content-Type` comes back.
- The gateway listens on loopback and rejects a foreign `Host` header (DNS-rebinding guard). It also rejects any
  web-page `Origin`, so a site you visit can't use your gateway. Browser extensions and non-browser clients are
  allowed; `--allow-origin` adds pages such as `https://polkadot.js.org`.
- Logs name the chain, the method names, the node and the timing. Storage keys (they contain your account id),
  addresses, extrinsic bytes and other params are never logged, not even in error messages. A "method" that
  doesn't look like `namespace_name` is logged as `?`.

## Limits (read these)

- **It hides your IP, not your accounts.** The node still sees which accounts you query and which extrinsics you
  send, and everything on chain stays public. dotmix removes the IP link, not the on-chain one.
- **One exit IP, one websocket.** All requests leave through the same exit gateway, and a wallet keeps one websocket
  per chain open for its whole session, subscribing to every account it holds. The node can link those accounts to
  each other ("one user watches these five accounts"). For unlinkable sessions, run separate client identities
  (`dotmix nym setup --id other --port 1090`) and separate gateways.
- **A rejected extrinsic is remembered.** Sending the same junk bytes twice got `1012 Transaction is temporarily
  banned` the second time: the node's pool keeps a ban list by hash. That is normal node behaviour, but it means a
  re-sent transaction is linkable to its first attempt even from a different exit.
- **Latency is seconds.** In my live run a CLI command on a fresh mixnet stream took 3.9 to 7.0 s (most around 6 s,
  including DoH, TLS and the genesis check). Inside an open websocket a call took 0.6 to 0.9 s. `@polkadot/api`
  needed 7 to 15 s to connect, because it downloads the runtime metadata (0.9 to 1.4 MB) first. polkadot.js apps
  showed the Asset Hub explorer after 19 s. Fine for a wallet; slow for an indexer.
- **Clients that ignore your URL bypass it.** dotmix only protects requests that reach it. Price feeds, token logos,
  identity lookups to other chains, telemetry and the web page itself (for polkadot.js apps) are not covered.
- **Light clients are a different trade.** substrate-connect/smoldot don't trust an RPC node, but they connect to
  bootnodes and peers directly, with your IP. dotmix is for the RPC-node setup most wallets use.
- Not tested yet: Talisman, SubWallet and the polkadot{.js} extension UIs, PAPI and subxt. The websocket path
  accepts any JSON-RPC, so new `chainHead_v1_*` methods pass through, but I only exercised the legacy ones.

## Tests

`go vet ./... && go test ./...` runs offline against a fake mixnet: a recording SOCKS5 proxy, a DoH server and fake
TLS Substrate nodes (websocket and HTTP). The tests check that:

- every connection goes through the proxy, and the only name lookups are DoH over it;
- websocket requests, answers and subscription notifications pass through byte for byte, including a 300 KiB
  message, and POST bodies (single and batch) too;
- a node serving another chain is skipped before it sees a request, a chain whose only node is wrong fails with
  502, and a custom chain pins the genesis of its first node;
- failover covers connection errors, timeouts, 5xx, 429 and redirects (which are never followed);
- only `/` and `/<chain>` route; sub-paths, encoded paths and unknown chains are 404;
- request and response headers are stripped;
- the Host and Origin guards hold, for POST, preflight and websocket upgrades;
- with no proxy there is a 502 and no fallback, in both DNS modes, for websockets and POSTs, even when a direct
  connection would work;
- no storage keys, account ids, addresses or extrinsic bytes reach the logs, on success or failure;
- BLAKE2b, xxHash64/twox128, SS58 (one- and two-byte prefixes, checksum), storage keys, SCALE compact and
  `AccountInfo` decoding match known vectors.

I also broke each guard on purpose (26 mutations: Host and Origin guards, path routing, the genesis check on both
paths, trust on first use, wss-only upstreams, a planted clearnet fallback in the dialer and in the HTTP client,
local DNS, redirects, failover, timeouts, log redaction, header stripping, body checks, and more) and checked that a
test fails every time.

The live run against public mainnet nodes through the real Nym mixnet is in `examples/live-retest-2026-11-07.txt`.
