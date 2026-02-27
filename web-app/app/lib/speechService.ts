import * as wrtc from 'node-datachannel/polyfill';
for (const key in wrtc) {
  (global as Record<string, unknown>)[key] = (wrtc as Record<string, unknown>)[key];
}

import * as VIAM from "@viamrobotics/sdk";
import { SpeechClient } from "speech-service-api";
let client: VIAM.RobotClient | null = null;
let speech: SpeechClient | null = null;
async function getSpeechClient() {
  if (!speech) {
    client = await VIAM.createRobotClient({
      host: process.env.VIAM_HOST!,
      credentials: {
        type: "api-key",
        payload: process.env.VIAM_API_KEY!,
        authEntity: process.env.VIAM_API_KEY_ID!,
      },
      signalingAddress: "https://app.viam.com:443",
    });
    speech = new SpeechClient(client, "speechio");
  }
  return speech;
}
export async function announceOrder(pronunciation: string) {
  const s = await getSpeechClient();
  await s.say(`Order ready for ${pronunciation}`, true);
}
