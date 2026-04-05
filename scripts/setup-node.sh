#!/usr/bin/env bash
set -euo pipefail

# Interactive setup for viiwork node(s).
# Generates viiwork configs, docker-compose, and optionally downloads models.

echo "=== viiwork node setup ==="
echo ""

# Detect GPUs — count AMD devices via sysfs (most reliable)
GPU_COUNT=0
for card in /sys/class/drm/card*/device/vendor; do
    [ -f "$card" ] && grep -q "0x1002" "$card" 2>/dev/null && GPU_COUNT=$((GPU_COUNT + 1))
done
if [ "$GPU_COUNT" -eq 0 ] && command -v rocm-smi &>/dev/null; then
    GPU_COUNT=$(rocm-smi --showid --json 2>/dev/null | grep -c '"card' || echo 0)
fi
if [ "$GPU_COUNT" -eq 0 ]; then
    GPU_COUNT=$(ls /dev/dri/renderD* 2>/dev/null | wc -l)
fi
if [ "$GPU_COUNT" -eq 0 ]; then
    read -rp "Could not detect GPUs. How many Radeon VII cards? " GPU_COUNT
else
    echo "Detected ${GPU_COUNT} GPUs."
    read -rp "Use all ${GPU_COUNT}? (y/n, default y): " confirm
    if [[ "$confirm" == "n" ]]; then
        read -rp "How many GPUs to use? " GPU_COUNT
    fi
fi

# --- "I'm Feeling Lucky" — auto-discover trending models from HuggingFace ---
# Uses the HuggingFace API to find trending GGUF models that fit in 16GB VRAM,
# picks a diverse assortment, and auto-generates a multi-instance experiment config.
VRAM_CEILING=13000000000  # 13GB in bytes — safe ceiling for 16GB Radeon VII
VRAM_FLOOR=1000000000     # 1GB minimum — skip tiny files (mmproj, imatrix, etc.)

feeling_lucky() {
    local category="${1:-mix}"

    echo ""
    echo "=== I'm Feeling Lucky ==="
    echo ""

    if ! command -v jq &>/dev/null; then
        echo "ERROR: jq is required for model discovery."
        echo "Install with: sudo apt install jq"
        return 1
    fi

    # How many distinct models to pick (max 5, or GPU count if fewer)
    local target_count=$((GPU_COUNT < 5 ? GPU_COUNT : 5))
    local candidates=()
    local seen_families=()

    # --- Primary path: llmfit (smart scoring + hardware-aware recommendations) ---
    if command -v llmfit &>/dev/null; then
        echo "Using llmfit for hardware-aware model discovery..."

        # Map category to llmfit --use-case value
        local use_case_flag=""
        case "$category" in
            c|coding)    use_case_flag="--use-case coding";     echo "  Category: coding" ;;
            r|reasoning) use_case_flag="--use-case reasoning";  echo "  Category: reasoning" ;;
            v|vision)    use_case_flag="--use-case multimodal"; echo "  Category: multimodal" ;;
            w|writing)   use_case_flag="--use-case chat";       echo "  Category: chat/writing" ;;
            l|multilingual) use_case_flag="--use-case chat";    echo "  Category: multilingual" ;;
            a|agents)    use_case_flag="--use-case coding";     echo "  Category: agents" ;;
            *)           echo "  Category: all" ;;
        esac

        # Query llmfit for models that fit 16GB VRAM with llama.cpp runtime
        local llmfit_json
        llmfit_json=$(llmfit --memory=16G recommend \
            --force-runtime llamacpp --min-fit perfect \
            -n 50 $use_case_flag --json 2>/dev/null) || true

        # Extract models under our VRAM ceiling that have GGUF sources
        local llmfit_models
        llmfit_models=$(echo "$llmfit_json" | jq -r --argjson max 14 '
            [.models[] | select(.memory_required_gb <= $max)
                       | select(.gguf_sources | length > 0)
                       | select(.best_quant | test("^Q|^IQ"; "i"))]
            | sort_by(.score) | reverse | .[] | "\(.name)|\(.score)|\(.gguf_sources[0].repo)"
        ' 2>/dev/null)

        echo ""

        for entry in $llmfit_models; do
            [ ${#candidates[@]} -ge $target_count ] && break

            IFS='|' read -r model_name score gguf_repo <<< "$entry"

            # Diversity: extract family name
            local family
            family=$(echo "$model_name" | sed 's|.*/||' | sed 's/-[0-9]*[bB].*$//i' | tr '[:upper:]' '[:lower:]')
            local skip=false
            for seen in "${seen_families[@]}"; do
                [[ "$seen" == "$family" ]] && skip=true && break
            done
            $skip && continue

            # Resolve actual GGUF file from the repo
            local files best
            files=$(curl -sf "https://huggingface.co/api/models/${gguf_repo}/tree/main" 2>/dev/null) || continue
            best=$(echo "$files" | jq -r --argjson max "$VRAM_CEILING" --argjson min "$VRAM_FLOOR" '
                [.[] | select(.type == "file") | select(.path | test("\\.gguf$"))
                     | select(.path | test("mmproj|imatrix"; "i") | not)
                     | select(.size <= $max and .size >= $min)]
                | sort_by(.size) | reverse | .[0] | "\(.path)|\(.size)"
            ' 2>/dev/null)

            [[ -z "$best" || "$best" == "null|null" ]] && continue

            local file_path file_size size_gb
            file_path=$(echo "$best" | cut -d'|' -f1)
            file_size=$(echo "$best" | cut -d'|' -f2)
            size_gb=$(awk "BEGIN {printf \"%.1f\", $file_size/1073741824}")

            candidates+=("${gguf_repo}|${file_path}|${size_gb}")
            seen_families+=("$family")
            echo "  + ${model_name} (score: ${score})"
            echo "    ${gguf_repo} → ${file_path} (${size_gb} GB)"
        done

        if [ ${#candidates[@]} -gt 0 ]; then
            echo ""
            echo "  Found ${#candidates[@]} model(s) via llmfit."
        fi
    fi

    # --- Fallback: HuggingFace API (if llmfit unavailable or found too few) ---
    if [ ${#candidates[@]} -lt $target_count ]; then
        [ ${#candidates[@]} -gt 0 ] && echo "  Supplementing with HuggingFace trending..."
        [ ${#candidates[@]} -eq 0 ] && echo "Discovering models from HuggingFace API..."

        # Map category to HF API search term and name filter regex
        local hf_search="" name_filter=""
        case "$category" in
            c|coding)       hf_search="coder";        name_filter="code|coder|starcoder|devstral|codestral|deepcoder" ;;
            r|reasoning)    hf_search="reason";        name_filter="r1|reason|think|math|deepseek.*r1|qwen.*math|skywork" ;;
            l|multilingual) hf_search="multilingual";  name_filter="aya|bloom|emma|seamless|madlad|glot|nllb|mistral.*instruct" ;;
            v|vision)       hf_search="vision";        name_filter="vision|vl|llava|pixtral|molmo|minicpm.*v|internvl|paligemma" ;;
            w|writing)      hf_search="instruct";      name_filter="instruct|chat|gemma.*it|llama.*instruct|mistral.*instruct|yi.*chat" ;;
            a|agents)       hf_search="function";      name_filter="agent|devstral|hermes|functionary|gorilla|nexus|tool" ;;
        esac

        local api_url="https://huggingface.co/api/models?filter=gguf&sort=downloads&direction=-1&limit=200"
        [ -n "$hf_search" ] && api_url="${api_url}&search=${hf_search}"

        local api_response
        api_response=$(curl -sf "$api_url") || {
            echo "ERROR: Failed to reach HuggingFace API."
            [ ${#candidates[@]} -eq 0 ] && return 1
        }

        # Extract unsloth repo IDs with 10+ likes
        local repo_ids
        if [ -n "$name_filter" ]; then
            repo_ids=$(echo "$api_response" | jq -r --arg pat "$name_filter" '
                [.[] | select(.id | test("GGUF$"; "i")) | select(.id | test("unsloth/"; "i"))
                     | select(.likes >= 10) | select(.id | test($pat; "i"))] | .[].id
            ')
            if [ -z "$repo_ids" ]; then
                repo_ids=$(echo "$api_response" | jq -r '
                    [.[] | select(.id | test("GGUF$"; "i")) | select(.id | test("unsloth/"; "i"))
                         | select(.likes >= 10)] | .[].id
                ')
            fi
        else
            repo_ids=$(echo "$api_response" | jq -r '
                [.[] | select(.id | test("GGUF$"; "i")) | select(.id | test("unsloth/"; "i"))
                     | select(.likes >= 10)] | .[].id
            ')
        fi

        echo ""

        for repo in $repo_ids; do
            [ ${#candidates[@]} -ge $target_count ] && break

            local family
            family=$(echo "$repo" | sed 's|.*/||' | sed 's/-GGUF$//i' | sed 's/-it$//i' | sed 's/-Instruct$//i' | \
                     sed 's/-[0-9]*[bB].*$//' | tr '[:upper:]' '[:lower:]')
            local skip=false
            for seen in "${seen_families[@]}"; do
                [[ "$seen" == "$family" ]] && skip=true && break
            done
            $skip && continue

            local files best
            files=$(curl -sf "https://huggingface.co/api/models/${repo}/tree/main" 2>/dev/null) || continue
            best=$(echo "$files" | jq -r --argjson max "$VRAM_CEILING" --argjson min "$VRAM_FLOOR" '
                [.[] | select(.type == "file") | select(.path | test("\\.gguf$"))
                     | select(.path | test("mmproj|imatrix"; "i") | not)
                     | select(.size <= $max and .size >= $min)]
                | sort_by(.size) | reverse | .[0] | "\(.path)|\(.size)"
            ' 2>/dev/null)

            [[ -z "$best" || "$best" == "null|null" ]] && continue

            local file_path file_size size_gb
            file_path=$(echo "$best" | cut -d'|' -f1)
            file_size=$(echo "$best" | cut -d'|' -f2)
            size_gb=$(awk "BEGIN {printf \"%.1f\", $file_size/1073741824}")

            candidates+=("${repo}|${file_path}|${size_gb}")
            seen_families+=("$family")
            echo "  + ${repo}"
            echo "    ${file_path} (${size_gb} GB)"
        done
    fi

    if [ ${#candidates[@]} -eq 0 ]; then
        echo ""
        echo "No suitable models found. Try manual selection."
        return 1
    fi

    echo ""
    echo "Found ${#candidates[@]} diverse models. Assigning GPUs..."

    # Round-robin GPU assignment across discovered models
    local gpus_per_model=$((GPU_COUNT / ${#candidates[@]}))
    local extra_gpus=$((GPU_COUNT % ${#candidates[@]}))
    local idx=0

    for candidate in "${candidates[@]}"; do
        IFS='|' read -r repo file size_gb <<< "$candidate"

        local gpus=$gpus_per_model
        [ $idx -lt $extra_gpus ] && gpus=$((gpus + 1))

        # Register as dynamic model entries (indices 20+)
        local num=$((20 + idx))
        MODEL_NAMES[$num]="$(echo "$repo" | sed 's|.*/||' | sed 's/-GGUF$//i')"
        MODEL_REPOS[$num]="$repo"
        MODEL_FILES[$num]="$file"
        MODEL_CTX[$num]=8192  # conservative for large models on 16GB

        INSTANCES+=("${num}:${gpus}:${GPU_OFFSET}:$((BASE_PORT + INSTANCE_NUM))")
        GPU_OFFSET=$((GPU_OFFSET + gpus))
        INSTANCE_NUM=$((INSTANCE_NUM + 1))
        GPUS_REMAINING=$((GPUS_REMAINING - gpus))
        idx=$((idx + 1))
    done
}

echo ""
echo "Available models (all fit in 16GB Radeon VII VRAM):"
echo ""
echo "  CODING:"
echo "  1) Qwen2.5-Coder-14B-Instruct (Q6_K, ~12.1GB) - best quality coding model"
echo "  2) Devstral-Small-24B (Q3_K_M, ~11.5GB) - multi-file frontend, agent workflows"
echo "  3) DeepSeek-R1-Distill-Qwen-14B (Q4_K_M, ~9GB) - algorithmic reasoning"
echo "  4) Qwen2.5-Coder-32B-Instruct (Q2_K, ~12.3GB) - largest coder, aggressive quant"
echo ""
echo "  TEXT & REASONING:"
echo "  5) Qwen3-32B (UD-Q2_K_XL, ~12.8GB) - general reasoning, thinking mode"
echo "  6) Gemma-3-27B-IT (Q3_K_S, ~12.2GB) - summarization, structured-to-prose"
echo "  7) Mistral-Small-3.1-24B-Instruct (IQ4_XS, ~12.8GB) - multilingual, instruction"
echo ""
echo "  GEMMA 4:"
echo "  8) Gemma-4-26B-A4B-IT (UD-Q3_K_M, ~12.5GB) - MoE, best quality that fits"
echo "  9) Gemma-4-26B-A4B-IT (UD-IQ3_S, ~11.2GB) - MoE, extra KV cache headroom"
echo "  10) Gemma-4-E4B-IT (Q8_0, ~8.2GB) - 8B multimodal, high quality quant"
echo "  11) Gemma-4-E2B-IT (Q8_0, ~5GB) - 5B multimodal, ultra-lightweight"
echo ""
echo "  DATA SCIENCE:"
echo "  12) DeepSeek-R1-Distill-Qwen-32B (Q2_K, ~12.3GB) - chain-of-thought, math"
echo ""
echo "  TRANSLATION & MULTILINGUAL:"
echo "  13) Qwen2.5-7B-Instruct (Q8_0, ~7.5GB) - strong multilingual, lightweight"
echo "  14) Qwen2.5-14B-Instruct (Q6_K, ~11.3GB) - strong multilingual, high quality"
echo "  15) Mistral-Nemo-Instruct-12B (Q6_K, ~9.4GB) - good European languages"
echo ""
echo "  OTHER:"
echo "  16) Custom (enter HuggingFace repo and filename)"
echo ""
echo "  FAMILIES (auto-distribute GPUs across all models in group):"
echo "  code) All coding      text) All text & reasoning"
echo "  g4)   All Gemma 4     data) Data science"
echo "  trans) Translation    all) Everything"
echo ""
echo "  DISCOVER (requires jq; optionally llmfit for smarter picks):"
echo "  0) I'm feeling lucky — trending models, any category"
echo "  0c) Coding  0r) Reasoning  0l) Multilingual"
echo "  0v) Vision  0w) Writing    0a) Agents"
echo ""

# Model definitions: name, HF repo, filename, context default
declare -A MODEL_NAMES MODEL_REPOS MODEL_FILES MODEL_CTX
MODEL_NAMES[1]="Qwen2.5-Coder-14B-Instruct"
MODEL_REPOS[1]="Qwen/Qwen2.5-Coder-14B-Instruct-GGUF"
MODEL_FILES[1]="qwen2.5-coder-14b-instruct-q6_k.gguf"
MODEL_CTX[1]=32768

MODEL_NAMES[2]="Devstral-Small-24B"
MODEL_REPOS[2]="unsloth/Devstral-Small-2507-GGUF"
MODEL_FILES[2]="Devstral-Small-2507-Q3_K_M.gguf"
MODEL_CTX[2]=32768

MODEL_NAMES[3]="DeepSeek-R1-Distill-Qwen-14B"
MODEL_REPOS[3]="bartowski/DeepSeek-R1-Distill-Qwen-14B-GGUF"
MODEL_FILES[3]="DeepSeek-R1-Distill-Qwen-14B-Q4_K_M.gguf"
MODEL_CTX[3]=32768

MODEL_NAMES[4]="Qwen2.5-Coder-32B-Instruct"
MODEL_REPOS[4]="unsloth/Qwen2.5-Coder-32B-Instruct-GGUF"
MODEL_FILES[4]="Qwen2.5-Coder-32B-Instruct-Q2_K.gguf"
MODEL_CTX[4]=8192

MODEL_NAMES[5]="Qwen3-32B"
MODEL_REPOS[5]="unsloth/Qwen3-32B-GGUF"
MODEL_FILES[5]="Qwen3-32B-UD-Q2_K_XL.gguf"
MODEL_CTX[5]=4096

MODEL_NAMES[6]="Gemma-3-27B-IT"
MODEL_REPOS[6]="unsloth/gemma-3-27b-it-GGUF"
MODEL_FILES[6]="gemma-3-27b-it-Q3_K_S.gguf"
MODEL_CTX[6]=32768

MODEL_NAMES[7]="Mistral-Small-3.1-24B-Instruct"
MODEL_REPOS[7]="unsloth/Mistral-Small-3.1-24B-Instruct-2503-GGUF"
MODEL_FILES[7]="Mistral-Small-3.1-24B-Instruct-2503-IQ4_XS.gguf"
MODEL_CTX[7]=32768

MODEL_NAMES[8]="Gemma-4-26B-A4B-IT"
MODEL_REPOS[8]="unsloth/gemma-4-26B-A4B-it-GGUF"
MODEL_FILES[8]="gemma-4-26B-A4B-it-UD-Q3_K_M.gguf"
MODEL_CTX[8]=4096

MODEL_NAMES[9]="Gemma-4-26B-A4B-IT-Light"
MODEL_REPOS[9]="unsloth/gemma-4-26B-A4B-it-GGUF"
MODEL_FILES[9]="gemma-4-26B-A4B-it-UD-IQ3_S.gguf"
MODEL_CTX[9]=4096

MODEL_NAMES[10]="Gemma-4-E4B-IT"
MODEL_REPOS[10]="unsloth/gemma-4-E4B-it-GGUF"
MODEL_FILES[10]="gemma-4-E4B-it-Q8_0.gguf"
MODEL_CTX[10]=32768

MODEL_NAMES[11]="Gemma-4-E2B-IT"
MODEL_REPOS[11]="unsloth/gemma-4-E2B-it-GGUF"
MODEL_FILES[11]="gemma-4-E2B-it-Q8_0.gguf"
MODEL_CTX[11]=32768

MODEL_NAMES[12]="DeepSeek-R1-Distill-Qwen-32B"
MODEL_REPOS[12]="unsloth/DeepSeek-R1-Distill-Qwen-32B-GGUF"
MODEL_FILES[12]="DeepSeek-R1-Distill-Qwen-32B-Q2_K.gguf"
MODEL_CTX[12]=4096

MODEL_NAMES[13]="Qwen2.5-7B-Instruct"
MODEL_REPOS[13]="bartowski/Qwen2.5-7B-Instruct-GGUF"
MODEL_FILES[13]="Qwen2.5-7B-Instruct-Q8_0.gguf"
MODEL_CTX[13]=32768

MODEL_NAMES[14]="Qwen2.5-14B-Instruct"
MODEL_REPOS[14]="bartowski/Qwen2.5-14B-Instruct-GGUF"
MODEL_FILES[14]="Qwen2.5-14B-Instruct-Q6_K.gguf"
MODEL_CTX[14]=32768

MODEL_NAMES[15]="Mistral-Nemo-Instruct-12B"
MODEL_REPOS[15]="bartowski/Mistral-Nemo-Instruct-2407-GGUF"
MODEL_FILES[15]="Mistral-Nemo-Instruct-2407-Q6_K.gguf"
MODEL_CTX[15]=32768

# ── Family definitions (shortcode → model indices) ──────────────────────────
declare -A FAMILY_MODELS
FAMILY_MODELS[code]="1 2 3 4"
FAMILY_MODELS[text]="5 6 7"
FAMILY_MODELS[g4]="8 9 10 11"
FAMILY_MODELS[data]="12"
FAMILY_MODELS[trans]="13 14 15"
FAMILY_MODELS[all]="1 2 3 4 5 6 7 8 9 10 11 12 13 14 15"

# Assign a family of models. Asks whether to spread all GPUs or use 1 each.
assign_family() {
    local family="$1"
    local model_ids=(${FAMILY_MODELS[$family]})
    local count=${#model_ids[@]}

    # Cap to available GPUs (need at least 1 GPU per model)
    if [ "$count" -gt "$GPUS_REMAINING" ]; then
        echo "  Only ${GPUS_REMAINING} GPUs — picking top ${GPUS_REMAINING} models from family."
        model_ids=("${model_ids[@]:0:$GPUS_REMAINING}")
        count=${#model_ids[@]}
    fi

    # Ask: spread all GPUs, or 1 each (leaving rest for later)?
    local gpus_per extra
    if [ "$GPUS_REMAINING" -gt "$count" ]; then
        read -rp "  1 GPU each (${count} used, ${GPUS_REMAINING} available) or spread all? [1/all, default 1]: " spread
        if [[ "$spread" == "all" ]]; then
            gpus_per=$((GPUS_REMAINING / count))
            extra=$((GPUS_REMAINING % count))
        else
            gpus_per=1
            extra=0
        fi
    else
        gpus_per=1
        extra=0
    fi

    local idx=0
    for mid in "${model_ids[@]}"; do
        local gpus=$gpus_per
        [ $idx -lt $extra ] && gpus=$((gpus + 1))

        INSTANCES+=("${mid}:${gpus}:${GPU_OFFSET}:$((BASE_PORT + INSTANCE_NUM))")
        echo "  Port $((BASE_PORT + INSTANCE_NUM)): ${MODEL_NAMES[$mid]} on ${gpus} GPUs"
        GPU_OFFSET=$((GPU_OFFSET + gpus))
        INSTANCE_NUM=$((INSTANCE_NUM + 1))
        GPUS_REMAINING=$((GPUS_REMAINING - gpus))
        idx=$((idx + 1))
    done
}

# Collect model selections
INSTANCES=()
GPUS_REMAINING=$GPU_COUNT
INSTANCE_NUM=0
BASE_PORT=8080
GPU_OFFSET=0

echo "You have ${GPU_COUNT} GPUs. Assign models to GPU groups."
echo ""
echo "  Enter a number (1-16) for a single model"
echo "  Enter a family: code, text, g4, data, all"
echo "  Enter 0/0c/0r/0v/0w/0l/0a for 'I'm feeling lucky'"
echo ""

# ── First selection ─────────────────────────────────────────────────────────
read -rp "Selection: " first_choice

if [[ "$first_choice" == 0* ]]; then
    # Feeling lucky
    lucky_category="${first_choice#0}"
    feeling_lucky "${lucky_category:-mix}" || true
elif [[ -n "${FAMILY_MODELS[$first_choice]+x}" ]]; then
    # Family selection
    echo ""
    echo "=== Family: ${first_choice} ==="
    assign_family "$first_choice"
else
    # Single model — enter manual loop
    :
fi

# ── Manual selection loop (for single picks or if family/lucky left GPUs) ───
while [ "$GPUS_REMAINING" -gt 0 ]; do
    # If first_choice was a single model number, use it on first iteration
    if [[ -n "${first_choice:-}" ]] && [[ ! "$first_choice" == 0* ]] && [[ -z "${FAMILY_MODELS[$first_choice]+x}" ]]; then
        choice="$first_choice"
        first_choice=""
    else
        # Offer to fill remaining GPUs
        if [ ${#INSTANCES[@]} -gt 0 ]; then
            echo ""
            echo "${GPUS_REMAINING} GPUs remaining."
            read -rp "Add more? (model #, family, 0=lucky, n=done): " choice
            [[ "$choice" == "n" || -z "$choice" ]] && break
        else
            break  # nothing selected and not a single model — shouldn't happen
        fi
    fi

    # Handle the choice
    if [[ "$choice" == 0* ]]; then
        lucky_category="${choice#0}"
        feeling_lucky "${lucky_category:-mix}" || true
        continue
    elif [[ -n "${FAMILY_MODELS[$choice]+x}" ]]; then
        echo ""
        echo "=== Family: ${choice} ==="
        assign_family "$choice"
        continue
    fi

    # Single model selection
    if [ "$choice" = "16" ]; then
        read -rp "  HuggingFace repo (e.g. user/model-GGUF): " custom_repo
        read -rp "  Filename (e.g. model-q4_k_m.gguf): " custom_file
        MODEL_REPOS[16]="$custom_repo"
        MODEL_FILES[16]="$custom_file"
        MODEL_NAMES[16]="Custom"
        MODEL_CTX[16]=32768
    fi

    if [ "$GPUS_REMAINING" -eq "$GPU_COUNT" ] && [ "$GPUS_REMAINING" -le 4 ]; then
        default_gpus=$GPUS_REMAINING
    else
        default_gpus=$((GPUS_REMAINING > 1 ? GPUS_REMAINING / 2 : GPUS_REMAINING))
    fi
    read -rp "  GPUs for ${MODEL_NAMES[$choice]}? (${GPUS_REMAINING} available, default ${default_gpus}): " gpu_count
    gpu_count="${gpu_count:-$default_gpus}"

    if [ "$gpu_count" -gt "$GPUS_REMAINING" ]; then
        echo "  Only ${GPUS_REMAINING} GPUs available!"
        continue
    fi

    INSTANCES+=("${choice}:${gpu_count}:${GPU_OFFSET}:$((BASE_PORT + INSTANCE_NUM))")
    GPUS_REMAINING=$((GPUS_REMAINING - gpu_count))
    GPU_OFFSET=$((GPU_OFFSET + gpu_count))
    INSTANCE_NUM=$((INSTANCE_NUM + 1))
done

# Assign remaining GPUs to last instance if any left
if [ "$GPUS_REMAINING" -gt 0 ] && [ "${#INSTANCES[@]}" -gt 0 ]; then
    last="${INSTANCES[-1]}"
    IFS=: read -r lchoice lgpus loffset lport <<< "$last"
    lgpus=$((lgpus + GPUS_REMAINING))
    INSTANCES[-1]="${lchoice}:${lgpus}:${loffset}:${lport}"
    echo "Assigned remaining ${GPUS_REMAINING} GPUs to ${MODEL_NAMES[$lchoice]}."
fi

# Validate no GPU device is used by more than one instance
declare -A USED_GPUS
has_overlap=false
for inst in "${INSTANCES[@]}"; do
    IFS=: read -r choice gpus offset port <<< "$inst"
    for ((g=0; g<gpus; g++)); do
        dev=$((offset + g))
        if [[ -n "${USED_GPUS[$dev]+x}" ]]; then
            echo "ERROR: GPU ${dev} assigned to both port ${USED_GPUS[$dev]} and port ${port}"
            has_overlap=true
        fi
        USED_GPUS[$dev]="$port"
    done
done
if $has_overlap; then
    echo "Aborting — fix GPU assignments to avoid overlap."
    exit 1
fi

echo ""
echo "=== Configuration Summary ==="
for inst in "${INSTANCES[@]}"; do
    IFS=: read -r choice gpus offset port <<< "$inst"
    devs=""
    for ((g=0; g<gpus; g++)); do
        [ -n "$devs" ] && devs="${devs}, "
        devs="${devs}$((offset + g))"
    done
    echo "  Port ${port}: ${MODEL_NAMES[$choice]} on GPUs [${devs}]"
done
echo ""
read -rp "Proceed? (y/n): " confirm
[[ "$confirm" != "y" ]] && echo "Aborted." && exit 0

# Create directories
mkdir -p models

# Load HF_TOKEN from .env if not already set
if [ -z "${HF_TOKEN:-}" ] && [ -f .env ]; then
    HF_TOKEN=$(grep -oP '^HF_TOKEN=\K.*' .env 2>/dev/null || true)
    [ -n "$HF_TOKEN" ] && export HF_TOKEN
fi
if [ -z "${HF_TOKEN:-}" ]; then
    echo "Tip: set HF_TOKEN in .env for faster downloads (see .env.example)"
fi

# Download models
echo ""
for inst in "${INSTANCES[@]}"; do
    IFS=: read -r choice gpus offset port <<< "$inst"
    file="${MODEL_FILES[$choice]}"
    if [ -f "models/${file}" ]; then
        echo "==> ${file} already exists, skipping download."
    else
        echo "==> Downloading ${MODEL_NAMES[$choice]}..."
        if command -v hf &>/dev/null; then
            hf download "${MODEL_REPOS[$choice]}" "${file}" --local-dir models
        elif command -v huggingface-cli &>/dev/null; then
            huggingface-cli download "${MODEL_REPOS[$choice]}" "${file}" --local-dir models
        else
            echo "  huggingface-cli not found. Install with: pip install huggingface-hub"
            echo "  Then run: hf download ${MODEL_REPOS[$choice]} ${file} --local-dir models"
        fi
    fi
done

# Generate configs
echo ""
echo "==> Generating configuration files..."

# If single instance, generate simple config
if [ "${#INSTANCES[@]}" -eq 1 ]; then
    IFS=: read -r choice gpus offset port <<< "${INSTANCES[0]}"
    file="${MODEL_FILES[$choice]}"
    ctx="${MODEL_CTX[$choice]}"

    cat > viiwork.yaml <<EOF
server:
  host: 0.0.0.0
  port: ${port}

model:
  path: /models/${file}
  context_size: ${ctx}
  n_gpu_layers: -1

gpus:
  count: ${gpus}
  base_port: 9001
  # power_limit_watts: 180

backend:
  binary: llama-server
  extra_args: ["--reasoning-format", "deepseek"]

health:
  interval: 5s
  timeout: 3s
  max_failures: 3

balancer:
  strategy: adaptive
  latency_window: 30s
  high_load_threshold: $((gpus - 3 > 1 ? gpus - 3 : 1))
  max_in_flight_per_gpu: 4
EOF

    echo "  Generated viiwork.yaml (single instance)"

    cat > docker-compose.yaml <<EOF
services:
  viiwork:
    image: viiwork
    build: .
    container_name: viiwork
    network_mode: host
    devices:
      - /dev/kfd:/dev/kfd
      - /dev/dri:/dev/dri
    volumes:
      - ./models:/models
      - ./viiwork.yaml:/etc/viiwork/viiwork.yaml
    group_add:
      - video
      - render
    restart: unless-stopped
EOF

    echo "  Generated docker-compose.yaml (single instance, network_mode: host)"

else
    # Multiple instances: generate per-instance configs + docker-compose
    PEER_LIST=""
    for inst in "${INSTANCES[@]}"; do
        IFS=: read -r choice gpus offset port <<< "$inst"
        PEER_LIST="${PEER_LIST}    - localhost:${port}\n"
    done

    for inst in "${INSTANCES[@]}"; do
        IFS=: read -r choice gpus offset port <<< "$inst"
        file="${MODEL_FILES[$choice]}"
        ctx="${MODEL_CTX[$choice]}"
        cfg_name="viiwork-${port}.yaml"
        base_backend_port=$((9001 + offset))

        # Build explicit GPU device list: [offset, offset+1, ..., offset+gpus-1]
        devices_yaml=""
        for ((g=0; g<gpus; g++)); do
            devices_yaml="${devices_yaml}
    - $((offset + g))"
        done

        # Build peer list excluding self
        peers_yaml=""
        for other in "${INSTANCES[@]}"; do
            IFS=: read -r _ _ _ oport <<< "$other"
            if [ "$oport" != "$port" ]; then
                peers_yaml="${peers_yaml}
    - localhost:${oport}"
            fi
        done

        cat > "${cfg_name}" <<EOF
server:
  host: 0.0.0.0
  port: ${port}

model:
  path: /models/${file}
  context_size: ${ctx}
  n_gpu_layers: -1

gpus:
  devices:${devices_yaml}
  base_port: ${base_backend_port}
  # power_limit_watts: 180

backend:
  binary: llama-server
  extra_args: ["--reasoning-format", "deepseek"]

health:
  interval: 5s
  timeout: 3s
  max_failures: 3

balancer:
  strategy: adaptive
  latency_window: 30s
  high_load_threshold: $((gpus - 1 > 1 ? gpus - 1 : 1))
  max_in_flight_per_gpu: 4

peers:
  hosts:${peers_yaml}
  poll_interval: 10s
  timeout: 3s
EOF

        echo "  Generated ${cfg_name}"
    done

    # Generate docker-compose for multi-instance
    cat > docker-compose.yaml <<EOF
services:
EOF

    for inst in "${INSTANCES[@]}"; do
        IFS=: read -r choice gpus offset port <<< "$inst"
        file="${MODEL_FILES[$choice]}"
        name=$(echo "${file}" | sed 's/\.gguf//' | tr '[:upper:]' '[:lower:]' | tr '.' '-')

        cat >> docker-compose.yaml <<EOF
  viiwork-${port}:
    image: viiwork
    build: .
    container_name: viiwork-${port}
    network_mode: host
    devices:
      - /dev/kfd:/dev/kfd
      - /dev/dri:/dev/dri
    volumes:
      - ./models:/models
      - ./viiwork-${port}.yaml:/etc/viiwork/viiwork.yaml
    group_add:
      - video
      - render
    restart: unless-stopped

EOF
    done

    echo "  Generated docker-compose.yaml (${#INSTANCES[@]} instances, network_mode: host)"
fi

echo ""
echo "=== Done ==="
echo ""
if [ "${#INSTANCES[@]}" -eq 1 ]; then
    IFS=: read -r _ _ _ port <<< "${INSTANCES[0]}"
    echo "Start with: docker compose up -d"
    echo "Dashboard:  http://localhost:${port}/"
    echo "API:        http://localhost:${port}/v1/models"
else
    echo "Start with: docker compose up -d"
    echo ""
    for inst in "${INSTANCES[@]}"; do
        IFS=: read -r choice gpus offset port <<< "$inst"
        echo "  ${MODEL_NAMES[$choice]}: http://localhost:${port}/"
    done
    echo ""
    echo "Connect OpenCode to any instance — mesh routing handles the rest."
    echo "All models visible from any endpoint via /v1/models."
fi
