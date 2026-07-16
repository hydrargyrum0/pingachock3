# Reverse proxy deployed on Cloud Run in front of the Debian backend, for
# nodes whose direct access is blocked (see docs/ARCHITECTURE.md, "Domain
# fronting via Cloud Run"). Same base pattern as the user's existing
# novavpn_backend_proxy_gcp / vless-reverse images: stock nginx + one
# envsubst template, no custom binary.
FROM nginx:1.27-alpine

# The base image ships a default.conf listening on :80 (welcome page) -
# drop it so the only active server block is ours, on ${PORT}.
RUN rm -f /etc/nginx/conf.d/default.conf

# Enables the stock image's /docker-entrypoint.d/15-local-resolvers.envsh,
# which reads /etc/resolv.conf and exports NGINX_LOCAL_RESOLVERS - our
# template's `resolver` directive uses that instead of hardcoded public DNS
# (8.8.8.8 etc), which Cloud Run's network doesn't reliably let containers
# reach directly. Confirmed via real deploy: proxy_pass to a hostname
# failed in ~90ms (too fast to be a network timeout - a DNS resolver
# unreachable at :53 fails instantly) until switched to this.
ENV NGINX_ENTRYPOINT_LOCAL_RESOLVERS=1

COPY nginx.conf.template /etc/nginx/templates/nginx.conf.template
COPY 00-validate-env.sh /docker-entrypoint.d/00-validate-env.sh
RUN chmod +x /docker-entrypoint.d/00-validate-env.sh
