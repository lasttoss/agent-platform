# Two stages: Vite builds static files, nginx serves them and proxies the API paths to the
# gateway. The browser talks to one origin, so there is no CORS configuration to get wrong.
FROM node:22-alpine AS build
WORKDIR /app
COPY services/console/package.json services/console/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY services/console/ ./
RUN npm run build

FROM nginx:alpine
COPY services/console/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 80
