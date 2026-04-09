import type { NextConfig } from "next";

const isDev = process.env.NODE_ENV === "development";

const nextConfig: NextConfig = {
  output: "export",
  ...(!isDev && { assetPrefix: "./" }),
  images: { unoptimized: true },
};

export default nextConfig;
