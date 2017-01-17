#!/bin/bash

set -ev
export VERSION=$(printf $(cat VERSION))

docker login -u="$DOCKER_USERNAME" -p="$DOCKER_PASSWORD"
docker build -t pdok/registrator:$VERSION .
docker push pdok/registrator:$VERSION
