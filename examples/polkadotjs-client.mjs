// An unmodified @polkadot/api client talking to the dotmix gateway.
// It knows nothing about Nym: only the endpoint URL differs.
//
//   npm install @polkadot/api
//   dotmix serve &
//   node polkadotjs-client.mjs [ws://127.0.0.1:9944]
import { ApiPromise, WsProvider } from '@polkadot/api';

const base = process.argv[2] || 'ws://127.0.0.1:9944';
const TREASURY = '13UVJyLnbVp9RBZYFwFGyDvVd1y27Tt8tkntv6Q7JVPhFsTB';
const t = () => performance.now();
const s = (t0) => ((t() - t0) / 1000).toFixed(1) + ' s';

async function connect(path) {
  const t0 = t();
  const provider = new WsProvider(base + path, 2500, {}, 180000);
  const api = await ApiPromise.create({ provider, noInitWarn: true, throwOnConnect: true });
  api.ws = provider;
  console.log(`${path || '/'}: connected, ${api.runtimeChain} ${api.runtimeVersion.specName}/${api.runtimeVersion.specVersion}, metadata v${api.runtimeMetadata.version} loaded (${s(t0)})`);
  return api;
}

const ah = await connect('/assethub');
let t0 = t();
const acct = await ah.query.system.account(TREASURY);
console.log(`assethub system.account(treasury): free ${acct.data.free.toHuman()} planck-free=${acct.data.free.toString()} nonce ${acct.nonce} (${s(t0)})`);
t0 = t();
const usdt = await ah.query.assets.account(1984, TREASURY);
console.log(`assethub assets.account(1984 USDT, treasury): ${usdt.isSome ? usdt.unwrap().balance.toString() : 'none'} (${s(t0)})`);
t0 = t();
const meta = await ah.query.assets.metadata(1984);
console.log(`assethub assets.metadata(1984): ${meta.symbol.toHuman()} decimals ${meta.decimals} (${s(t0)})`);
t0 = t();
try {
  // Raw provider call: api.rpc.author.submitExtrinsic would decode the junk
  // locally and never send it.
  await ah.ws.send('author_submitExtrinsic', ['0x1084c0ffee']);
  console.log('author_submitExtrinsic(junk): accepted?!');
} catch (e) {
  console.log(`author_submitExtrinsic(junk bytes): rejected by the node: ${String(e.message).split('\n')[0].slice(0, 90)} (${s(t0)})`);
}
t0 = t();
await new Promise(async (resolve) => {
  let n = 0;
  const unsub = await ah.rpc.chain.subscribeNewHeads((h) => {
    console.log(`assethub new head #${h.number} (${s(t0)} after subscribing)`);
    if (++n === 2) { unsub(); resolve(); }
  });
});
await ah.disconnect();

const dot = await connect('');
t0 = t();
const [chain, fin] = await Promise.all([dot.rpc.system.chain(), dot.rpc.chain.getFinalizedHead()]);
const hdr = await dot.rpc.chain.getHeader(fin);
console.log(`polkadot (gateway root): ${chain}, finalized #${hdr.number} (${s(t0)})`);
await dot.disconnect();
process.exit(0);
