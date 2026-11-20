#!/bin/sh

set -eu

git pull --ff-only
docker compose pull
docker compose up -d
