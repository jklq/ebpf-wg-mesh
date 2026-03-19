FROM oven/bun:1.3.11-slim AS build

WORKDIR /app/dashboard

COPY dashboard/package.json dashboard/bun.lock ./
RUN bun install --frozen-lockfile

COPY dashboard ./
COPY api/proto /app/api/proto

RUN bun run build

FROM oven/bun:1.3.11-slim

ENV NODE_ENV=production
ENV HOST=0.0.0.0

WORKDIR /app/dashboard

COPY --from=build /app/dashboard/package.json /app/dashboard/bun.lock ./
COPY --from=build /app/dashboard/.output ./.output
COPY --from=build /app/api /app/api

EXPOSE 3000

CMD ["bun", "run", "start"]
