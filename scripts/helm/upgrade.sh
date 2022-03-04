#!/usr/bin/env bash
set -eufo pipefail

command -v kubectl >/dev/null 2>&1 || { echo "kubectl not installed,  Aborting." >&2; exit 1; }
command -v helm >/dev/null 2>&1 || { echo "helm not installed,  Aborting." >&2; exit 1; }

export POSTGRESQL_PASSWORD=$(kubectl get secret --namespace "jackal" jackal-postgresql-ha-postgresql -o jsonpath="{.data.postgresql-password}" | base64 --decode)
export REPMGR_PASSWORD=$(kubectl get secret --namespace "jackal" jackal-postgresql-ha-postgresql -o jsonpath="{.data.repmgr-password}" | base64 --decode)
export ADMIN_PASSWORD=$(kubectl get secret --namespace "jackal" jackal-postgresql-ha-pgpool -o jsonpath="{.data.admin-password}" | base64 --decode)

export REDIS_PASSWORD=$(kubectl get secret --namespace "jackal" jackal-redis -o jsonpath="{.data.redis-password}" | base64 --decode)

helm upgrade jackal helm/jackal/ --dependency-update \
--set postgresql-ha.postgresql.password=$POSTGRESQL_PASSWORD \
--set postgresql-ha.postgresql.repmgrPassword=$REPMGR_PASSWORD \
--set postgresql-ha.pgpool.adminPassword=$ADMIN_PASSWORD \
--set password=$REDIS_PASSWORD \
--namespace=jackal
