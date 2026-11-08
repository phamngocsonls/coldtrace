import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // standalone output -> tiny Bun runtime image (see Dockerfile)
  output: "standalone",
};

export default nextConfig;
