import * as wrtc from 'node-datachannel/polyfill';
for (const key in wrtc) {
  (global as Record<string, unknown>)[key] = (wrtc as Record<string, unknown>)[key];
}
import * as VIAM from '@viamrobotics/sdk';
async function test() {
  const client = await VIAM.createRobotClient({
    host: process.env.VIAM_HOST!,
    credentials: {
      type: 'api-key',
      payload: process.env.VIAM_API_KEY!,
      authEntity: process.env.VIAM_API_KEY_ID!,
    },
    signalingAddress: 'https://app.viam.com:443',
  });
  // List all resources to see what's available
  const resources = await client.resourceNames();
  console.log("Resources:", resources.map(r => `${r.namespace}:${r.type}:${r.subtype} - ${r.name}`));
  await client.disconnect();
}
test().catch(console.error);