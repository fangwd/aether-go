#!/bin/sh
# Runs once when the compose Postgres starts empty: cluster roles, then
# aether's database.
set -eu
psql -v ON_ERROR_STOP=1 -U postgres -v app_password=aether_app -v worker_password=aether_worker -f /bootstrap.sql
psql -v ON_ERROR_STOP=1 -U postgres -c "CREATE DATABASE aether"
