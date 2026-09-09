FROM node:26-alpine AS build
WORKDIR /src
COPY apps/web/ .
RUN npm ci && npm run build

FROM node:26-alpine
WORKDIR /app
COPY --from=build /src/.next/standalone ./
EXPOSE 3000
CMD ["node", "server.js"]
