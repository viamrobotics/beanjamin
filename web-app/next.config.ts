import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  /* config options here */
  transpilePackages: ['speech-service-api'],
  serverExternalPackages: ['node-datachannel'],
};

export default nextConfig;
