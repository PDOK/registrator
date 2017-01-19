#!/bin/bash

if [ -z "$TRAVIS_TAG" ]; then

    set -ev
    export VERSION=$(printf $(cat VERSION))

    docker login -u="$DOCKER_USERNAME" -p="$DOCKER_PASSWORD"
    docker build -t pdok/registrator:$TRAVIS_COMMIT -t pdok/registrator:latest .
    docker push pdok/registrator

fi