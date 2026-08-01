#!/bin/sh
# Inject secrets from Infisical, then hand off to the app.
#
# Mechanism per the brain's Global Agent Rules ("Infisical secrets in Docker
# containers", Docker Entrypoint method): a Universal Auth machine identity
# logs in at container start and the app runs under `infisical run`, which puts
# the project's secrets — RUNPOD_API_KEY — into its environment. The token is
# minted here rather than baked in, so it never lands in an image layer.
#
# With no client credentials present the app starts bare, so a developer can
# run it with RUNPOD_API_KEY set by hand.
set -eu

if [ -z "${INFISICAL_CLIENT_ID:-}" ] || [ -z "${INFISICAL_CLIENT_SECRET:-}" ]; then
	echo "entrypoint: no Infisical machine identity configured; starting without secret injection" >&2
	exec "$@"
fi

: "${INFISICAL_DOMAIN:?INFISICAL_DOMAIN is required when a machine identity is set}"
: "${INFISICAL_PROJECT_ID:?INFISICAL_PROJECT_ID is required when a machine identity is set}"
INFISICAL_ENV="${INFISICAL_ENV:-prod}"

echo "entrypoint: authenticating to Infisical at ${INFISICAL_DOMAIN}" >&2
INFISICAL_TOKEN="$(infisical login \
	--method=universal-auth \
	--client-id="${INFISICAL_CLIENT_ID}" \
	--client-secret="${INFISICAL_CLIENT_SECRET}" \
	--domain="${INFISICAL_DOMAIN}" \
	--plain --silent)"
export INFISICAL_TOKEN

echo "entrypoint: injecting ${INFISICAL_ENV} secrets and starting the app" >&2
exec infisical run \
	--projectId="${INFISICAL_PROJECT_ID}" \
	--env="${INFISICAL_ENV}" \
	--domain="${INFISICAL_DOMAIN}" \
	--silent \
	-- "$@"
