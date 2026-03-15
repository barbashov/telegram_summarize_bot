#!/bin/sh

set -eu

git pull
docker-compose pull
docker-compose down
docker-compose up -d --force-recreate

