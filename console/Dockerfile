FROM oven/bun:1.3.11-slim AS bun

FROM node:22-bookworm-slim AS build
RUN apt-get update && apt-get install --yes --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=bun /usr/local/bin/bun /usr/local/bin/bun
RUN ln -s /usr/local/bin/bun /usr/local/bin/bunx

WORKDIR /app/console

COPY console/package.json console/bun.lock ./
RUN bun install --frozen-lockfile

COPY console ./
COPY api/proto /app/api/proto

RUN bun run build

FROM oven/bun:1.3.11-slim

ENV NODE_ENV=production
ENV HOST=0.0.0.0

WORKDIR /app/console

COPY --from=build /app/console/package.json /app/console/bun.lock ./
COPY --from=build /app/console/.output ./.output
COPY --from=build /app/api /app/api

EXPOSE 3000

CMD ["bun", "run", "start"]
