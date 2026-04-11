#!/usr/bin/env bash
set -euo pipefail

OMLX_DIR="$HOME/source/oMLX"
CONDA_BASE="/opt/homebrew/Caskroom/miniconda/base"
ENV_NAME="omlx"

# --- replicated from ~/source/oMLX/start.sh (kept inline, not linked) ---
if ! "$CONDA_BASE/bin/conda" env list | grep -q "^${ENV_NAME} "; then
    echo "Creating conda environment '${ENV_NAME}' with Python 3.12..."
    "$CONDA_BASE/bin/conda" create -n "$ENV_NAME" python=3.12 -y
    echo "Installing oMLX dependencies from ${OMLX_DIR}..."
    "$CONDA_BASE/bin/conda" run -n "$ENV_NAME" pip install -e "$OMLX_DIR"
fi

# shellcheck disable=SC1091
source "$CONDA_BASE/etc/profile.d/conda.sh"
conda activate "$ENV_NAME"

echo "Starting oMLX server..."
echo "  Admin dashboard: http://127.0.0.1:8000/admin"
echo "  API endpoint:    http://127.0.0.1:8000/v1"
exec omlx serve
