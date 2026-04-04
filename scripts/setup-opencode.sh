#!/usr/bin/env bash
set -euo pipefail

# Install OpenCode and configure it to use a viiwork host.
# Auto-detects all models from the mesh endpoint.

read -rp "Viiwork host (IP or hostname, default localhost): " input
host="${input:-localhost}"

# Add http:// and :8080 if missing
[[ "$host" != http://* && "$host" != https://* ]] && host="http://$host"
[[ ! "$host" =~ :[0-9]+$ ]] && host="${host}:8080"

# Detect all models from mesh
MODELS=()
MODEL_NAMES=()
if curl -sf "${host}/v1/models" &>/dev/null; then
    while IFS= read -r id; do
        [ -n "$id" ] && MODELS+=("$id")
    done < <(curl -sf "${host}/v1/models" | grep -o '"id":"[^"]*"' | cut -d'"' -f4)

    if [ ${#MODELS[@]} -gt 0 ]; then
        echo "Detected ${#MODELS[@]} model(s):"
        for m in "${MODELS[@]}"; do
            echo "  - ${m}"
        done
    fi
else
    echo "Host not reachable at ${host}"
    read -rp "Enter model ID manually: " manual
    MODELS+=("$manual")
fi

if [ ${#MODELS[@]} -eq 0 ]; then
    echo "No models found. Aborting."
    exit 1
fi

# Install OpenCode
if ! command -v opencode &>/dev/null; then
    echo "Installing OpenCode..."
    curl -fsSL https://opencode.ai/install | bash
else
    echo "OpenCode already installed: $(command -v opencode)"
fi

# Build models JSON block
models_json=""
for m in "${MODELS[@]}"; do
    # Create a display name: strip quant suffix, replace hyphens with spaces, title case
    display=$(echo "$m" | sed 's/-q[0-9].*//; s/_q[0-9].*//' | tr '-' ' ' | tr '_' ' ')
    if [ -n "$models_json" ]; then
        models_json="${models_json},"
    fi
    models_json="${models_json}
        \"${m}\": {
          \"name\": \"${display}\"
        }"
done

# Default to first model
default_model="${MODELS[0]}"
if [ ${#MODELS[@]} -gt 1 ]; then
    echo ""
    echo "Which model should be the default?"
    for i in "${!MODELS[@]}"; do
        echo "  $((i+1))) ${MODELS[$i]}"
    done
    read -rp "Choice (default 1): " choice
    choice="${choice:-1}"
    default_model="${MODELS[$((choice-1))]}"
fi

# Write config
cat > opencode.json <<EOF
{
  "\$schema": "https://opencode.ai/config.json",
  "provider": {
    "viiwork": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Viiwork",
      "options": {
        "baseURL": "${host}/v1",
        "apiKey": "not-needed"
      },
      "models": {${models_json}
      }
    }
  },
  "model": {
    "default": "viiwork/${default_model}"
  }
}
EOF

echo ""
echo "Wrote opencode.json"
echo "  Host:    ${host}"
echo "  Models:  ${#MODELS[@]}"
echo "  Default: ${default_model}"
echo ""
echo "Run 'opencode' to start."
