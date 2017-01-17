#!/bin/bash

if [ -z "$TRAVIS_TAG" ]; then

    set -ev
    export VERSION=$(printf $(cat VERSION))

    docker login -u="$DOCKER_USERNAME" -p="$DOCKER_PASSWORD"
    docker build -t pdok/registrator:$TRAVIS_COMMIT .
    docker push pdok/registrator:$TRAVIS_COMMIT

fi