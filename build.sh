#!/bin/bash
echo "building: ${1}"
echo "my images: ${IMAGES}"
echo "push: ${PUSH_IMAGE}"

make ${1}-binary

export DOCKER_BUILDKIT=1
docker build -t ${IMAGES:?} -f Dockerfile.${1}.local .
${PUSH_IMAGE:?} && docker push $IMAGES
