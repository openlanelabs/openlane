FROM node:20-alpine AS build
WORKDIR /src
COPY apps/web/ .
RUN npm ci && npm run build

FROM node:20-alpine
WORKDIR /app
COPY --from=build /src/.next/standalone ./
EXPOSE 3000
CMD ["node", "server.js"]
