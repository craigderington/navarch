#!/usr/bin/env bash
# Build the marketing-site image and put it on every node's daemon.
#
# This is the operator side of a contract the platform states plainly: `build:`
# is a rejected compose directive, because a platform that builds cannot tell
# you what it deployed. Images are built and pushed, then referenced by name.
#
# In a real fleet that means a registry. Here each node is a separate Docker
# daemon inside DinD with no registry between them, so the image is streamed
# straight in — `docker save | docker load` is the registry, minus the registry.
# The agent pulls only when an image is absent, so a loaded image is simply used.
#
# Every node gets it because placement is scored: which node the stack lands on
# is the scheduler's decision, not ours.
set -euo pipefail
cd "$(dirname "$0")/.."

IMAGE=${IMAGE:-ghcr.io/craigderington/navarch/site:3}

step() { printf '\n\033[36m▸ %s\033[0m\n' "$1"; }
note() { printf '  \033[90m%s\033[0m\n' "$1"; }

step "Building $IMAGE"
docker build -q -t "$IMAGE" examples/site >/dev/null
note "built"

step "Loading it onto every node"
nodes=$(docker compose config --services | grep '^dind-' | sort)
for n in $nodes; do
    docker save "$IMAGE" | docker compose exec -T "$n" docker load >/dev/null
    note "$n"
done
