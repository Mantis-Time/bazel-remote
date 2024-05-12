#!/usr/bin/env bash
set -o pipefail -o errexit -o nounset

if [ "$#" -ne 1 ]; then
    echo "Usage: $0 <your_image>"
    exit 1
fi

IMAGE="$1"
pwd
docker push "$IMAGE"
