#!/usr/bin/env bash
set -euo pipefail

case ${1:-} in
    -h|--help)
        printf '%s\n' \
            'Usage: ./install.sh' \
            'Builds and installs all AID binaries to ~/.local/bin.' \
            'Set INSTALL_DIR to an absolute path to choose another directory.' \
            'Requires Go (see go.mod for the minimum version).'
        exit 0
        ;;
esac
if (( $# )); then
    printf 'error: unexpected arguments; use --help for usage.\n' >&2
    exit 2
fi

: "${INSTALL_DIR:=${HOME:?HOME must be set}/.local/bin}"
if [[ ${INSTALL_DIR} != /* ]]; then
    printf 'error: INSTALL_DIR must be an absolute path.\n' >&2
    exit 2
fi
if ! command -v go >/dev/null 2>&1; then
    printf 'error: Go is required; see go.mod for the minimum version.\n' >&2
    exit 1
fi

script_path=${BASH_SOURCE[0]}
script_dir=${script_path%/*}
[[ ${script_dir} != "${script_path}" ]] || script_dir=.
cd -- "${script_dir}"

# The builder stages and verifies every binary before replacing the installed suite.
# Clear cross-compilation overrides so the build driver itself runs on this host.
(
    unset GOOS GOARCH GOBIN
    go run ./cmd/aid-build --out "${INSTALL_DIR}"
)

printf 'Installed the AID suite to %s\n' "${INSTALL_DIR}"
case :${PATH:-}: in
    *:"${INSTALL_DIR}":*) ;;
    *)
        printf 'Add this directory to your PATH in your shell startup file:\n'
        printf 'export PATH=%q:%s\n' "${INSTALL_DIR}" "\"\$PATH\""
        ;;
esac
