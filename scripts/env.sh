#!/bin/sh
# Load the Infisical machine-identity credentials into the current shell, then
# bring the stack up:
#
#     . scripts/env.sh && docker compose up -d
#
# Config (host, client id, project id, environment) comes from a directory
# OUTSIDE this repository, so no identity or key material is ever committed.
# The client secret is never on disk in plaintext — it is decrypted from an
# encrypted store beside that config, on demand.
#
# Point INFISICAL_CONFIG / INFISICAL_SECRET_GPG elsewhere to use a different
# machine identity. Each service should have its own.

INFISICAL_CONFIG="${INFISICAL_CONFIG:-$HOME/.config/timbre/infisical.env}"
INFISICAL_SECRET_GPG="${INFISICAL_SECRET_GPG:-$HOME/.config/timbre/infisical_client_secret.gpg}"

if [ ! -f "$INFISICAL_CONFIG" ]; then
	echo "env.sh: $INFISICAL_CONFIG not found" >&2
	return 1 2>/dev/null || exit 1
fi

# Only the non-secret keys; the client secret comes from gpg below.
INFISICAL_HOST="$(sed -n 's/^INFISICAL_HOST=//p' "$INFISICAL_CONFIG")"
INFISICAL_CLIENT_ID="$(sed -n 's/^INFISICAL_CLIENT_ID=//p' "$INFISICAL_CONFIG")"
INFISICAL_PROJECT_ID="$(sed -n 's/^INFISICAL_PROJECT_ID=//p' "$INFISICAL_CONFIG")"
INFISICAL_ENV="$(sed -n 's/^INFISICAL_ENV=//p' "$INFISICAL_CONFIG")"
export INFISICAL_HOST INFISICAL_CLIENT_ID INFISICAL_PROJECT_ID
export INFISICAL_ENV="${INFISICAL_ENV:-prod}"

if [ -z "${INFISICAL_CLIENT_SECRET:-}" ]; then
	if [ ! -f "$INFISICAL_SECRET_GPG" ]; then
		echo "env.sh: $INFISICAL_SECRET_GPG not found and INFISICAL_CLIENT_SECRET unset" >&2
		return 1 2>/dev/null || exit 1
	fi
	INFISICAL_CLIENT_SECRET="$(gpg --batch --quiet --yes --pinentry-mode loopback \
		--decrypt "$INFISICAL_SECRET_GPG")"
	export INFISICAL_CLIENT_SECRET
fi

echo "env.sh: Infisical identity ${INFISICAL_CLIENT_ID} -> ${INFISICAL_HOST} (${INFISICAL_ENV})" >&2
