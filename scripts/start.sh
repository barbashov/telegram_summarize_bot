#!/bin/sh

docker-compose pull && docker-compose up -d --force-recreate
