# Ops Console — Next.js standalone.
# NOTE: `next build` SIGTRAP-crashes under oven/bun inside Docker (works fine
# natively on the host with bun). So: deps are installed with Bun (bun.lock is
# the source of truth), the build + runtime use Node LTS.
# Runtime stage is Debian (not Alpine): the prebuilt @next/swc linux-*-gnu
# binary needs glibc (on musl it fails with "__res_init: symbol not found").
# Serves on PORT 3301 (see docker-compose.yml).
FROM oven/bun:1.4.4-slim AS deps
WORKDIR /app
COPY package.json bun.lock ./
# The lockfile records a host-specific @next/swc binary for every platform.
# Strip ALL of them (any host: darwin/linux/win32, arm64/x64) so Bun installs
# the linux record for this image instead of the one the dev machine wrote.
# Hardcoding "darwin" here broke the build whenever the repo was cloned to a
# non-mac host.
RUN bun -e 'const f=Bun.file("bun.lock"); const s=await f.text(); await Bun.write("bun.lock", s.replace(/\n\s*"@next\/swc-[a-z0-9-]+": \["@next\/swc-[a-z0-9-]+@[^"]+", "[^"]*"\],?/g, ""))'
RUN bun install --frozen-lockfile

FROM node:24-slim AS build
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
ENV NEXT_TELEMETRY_DISABLED=1
RUN node node_modules/next/dist/bin/next build

# Runtime: Next standalone output on Node (matches the build engine)
FROM node:24-slim AS run
WORKDIR /app
ENV NODE_ENV=production NEXT_TELEMETRY_DISABLED=1 PORT=3301 HOSTNAME=0.0.0.0
COPY --from=build /app/.next/standalone ./
COPY --from=build /app/.next/static ./.next/static
COPY --from=build /app/public ./public
EXPOSE 3301
CMD ["node", "server.js"]
