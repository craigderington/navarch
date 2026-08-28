# The Navarch marketing site, as an image Navarch can deploy.
#
# Built here and distributed to the fleet by `make site-image`, because the
# platform does not build: `build:` is a rejected compose directive, on purpose.
# Pre-building and pushing is the contract, and this is what honouring it looks
# like from the operator's side.
FROM nginx:1.27-alpine

COPY html/ /usr/share/nginx/html/

# wget is what the healthcheck uses; busybox already provides it in the alpine
# base, so nothing extra is installed.
HEALTHCHECK --interval=5s --timeout=3s --retries=6 \
  CMD wget -qO- http://127.0.0.1/ >/dev/null || exit 1
