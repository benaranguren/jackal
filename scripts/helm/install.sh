#!/usr/bin/env bash
set -eufo pipefail

command -v helm >/dev/null 2>&1 || { echo "helm not installed,  Aborting." >&2; exit 1; }

HELM_FILES_DIR=helm/jackal/tmpfiles

FILES=(
  "sql/postgres.up.psql"
)

rm -rf "${HELM_FILES_DIR}"
mkdir "${HELM_FILES_DIR}"

for file in "${FILES[@]}"; do
  cp "${file}" "${HELM_FILES_DIR}"
done

helm install jackal helm/jackal/ --dependency-update --create-namespace --namespace=jackal

rm -rf "${HELM_FILES_DIR}"
